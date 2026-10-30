package main

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func validArchivePath(name string) bool {
	return name != "" && name != "." && name != ".." && path.Clean(name) == name && !strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\\\x00")
}
func main() {
	archive := flag.String("archive", "", "archive; defaults to dist/ModelGuard-<VERSION>.s2plugin")
	public := flag.String("public-key", "dist/publisher-public-key.txt", "public key file")
	allowUnsigned := flag.Bool("allow-unsigned", false, "accept an absent signature; still verify every declared file")
	flag.Parse()
	if *archive == "" {
		version, err := os.ReadFile("VERSION")
		must(err)
		*archive = fmt.Sprintf("dist/ModelGuard-%s.s2plugin", strings.TrimSpace(string(version)))
	}
	z, err := zip.OpenReader(*archive)
	must(err)
	defer z.Close()
	files := map[string][]byte{}
	var total uint64
	if len(z.File) == 0 || len(z.File) > 4096 {
		must(fmt.Errorf("invalid archive file count"))
	}
	for _, f := range z.File {
		_, duplicate := files[f.Name]
		if !validArchivePath(f.Name) || f.Mode()&os.ModeSymlink != 0 || f.FileInfo().IsDir() || duplicate {
			must(fmt.Errorf("unsafe archive path"))
		}
		if f.UncompressedSize64 > 64<<20 || f.UncompressedSize64 > (128<<20)-total {
			must(fmt.Errorf("package too large"))
		}
		total += f.UncompressedSize64
		r, err := f.Open()
		must(err)
		b, err := io.ReadAll(io.LimitReader(r, (64<<20)+1))
		closeErr := r.Close()
		must(err)
		must(closeErr)
		if len(b) > 64<<20 || uint64(len(b)) != f.UncompressedSize64 {
			must(fmt.Errorf("invalid archive entry size"))
		}
		files[f.Name] = b
	}
	var m struct {
		SchemaVersion int               `json:"schema_version"`
		ID            string            `json:"id"`
		Name          string            `json:"name"`
		Version       string            `json:"version"`
		Files         map[string]string `json:"files"`
		Runtimes      map[string]struct {
			Path string `json:"path"`
		} `json:"runtimes"`
		UI struct {
			Entrypoint string `json:"entrypoint"`
		} `json:"ui"`
	}
	if _, ok := files["manifest.json"]; !ok {
		must(fmt.Errorf("missing manifest.json"))
	}
	must(json.Unmarshal(files["manifest.json"], &m))
	// This is a bounded integrity check, not the full manifest JSON schema.
	// scripts/check-package.py also validates the schema with AJV.
	if m.SchemaVersion != 1 || strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.Version) == "" || len(m.Files) == 0 || len(m.Runtimes) == 0 {
		must(fmt.Errorf("invalid basic manifest fields"))
	}
	for _, runtime := range m.Runtimes {
		if _, ok := m.Files[runtime.Path]; !ok {
			must(fmt.Errorf("runtime entrypoint is not declared"))
		}
	}
	if m.UI.Entrypoint != "" {
		if _, ok := m.Files[m.UI.Entrypoint]; !ok {
			must(fmt.Errorf("UI entrypoint is not declared"))
		}
	}
	var sig struct {
		Algorithm string `json:"algorithm"`
		KeyID     string `json:"key_id"`
		Signature string `json:"signature"`
	}
	signatureStatus := "unsigned"
	if rawSignature, signed := files["signature.json"]; signed {
		// A malformed or invalid signature is never an unsigned package.
		must(json.Unmarshal(rawSignature, &sig))
		if sig.Algorithm != "ed25519" || strings.TrimSpace(sig.KeyID) == "" {
			must(fmt.Errorf("invalid signature algorithm or publisher id"))
		}
		raw, err := os.ReadFile(*public)
		must(err)
		pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		must(err)
		signature, err := base64.StdEncoding.DecodeString(sig.Signature)
		must(err)
		if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, files["manifest.json"], signature) {
			must(fmt.Errorf("signature verification failed"))
		}
		signatureStatus = "passed"
	} else if !*allowUnsigned {
		must(fmt.Errorf("unsigned package; pass -allow-unsigned to verify its file integrity"))
	}
	for name, hash := range m.Files {
		decodedHash, err := hex.DecodeString(hash)
		if !validArchivePath(name) || name == "manifest.json" || name == "signature.json" || err != nil || len(decodedHash) != sha256.Size {
			must(fmt.Errorf("invalid declared file %s", name))
		}
		b, ok := files[name]
		if !ok {
			must(fmt.Errorf("missing declared file %s", name))
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != hash {
			must(fmt.Errorf("hash mismatch %s", name))
		}
	}
	for name := range files {
		if name == "manifest.json" || name == "signature.json" {
			continue
		}
		if _, ok := m.Files[name]; !ok {
			must(fmt.Errorf("undeclared archive entry %s", name))
		}
	}
	result, _ := json.Marshal(map[string]any{"plugin_id": m.ID, "version": m.Version, "publisher": sig.KeyID, "signature": signatureStatus, "file_hashes": "passed", "manifest_fields": "passed", "files": len(m.Files), "uncompressed_bytes": total})
	fmt.Println(string(result))
}
