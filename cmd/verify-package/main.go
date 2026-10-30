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
func main() {
	archive := flag.String("archive", "dist/ModelGuard-0.2.1.s2plugin", "archive")
	public := flag.String("public-key", "dist/publisher-public-key.txt", "public key file")
	flag.Parse()
	z, err := zip.OpenReader(*archive)
	must(err)
	defer z.Close()
	files := map[string][]byte{}
	var total uint64
	for _, f := range z.File {
		if path.Clean(f.Name) != f.Name || strings.HasPrefix(f.Name, "/") || strings.Contains(f.Name, "../") || f.Mode()&os.ModeSymlink != 0 || files[f.Name] != nil {
			must(fmt.Errorf("unsafe archive path"))
		}
		total += f.UncompressedSize64
		if total > 128<<20 {
			must(fmt.Errorf("package too large"))
		}
		r, err := f.Open()
		must(err)
		b, err := io.ReadAll(io.LimitReader(r, 64<<20))
		r.Close()
		must(err)
		files[f.Name] = b
	}
	var m struct {
		ID      string            `json:"id"`
		Version string            `json:"version"`
		Files   map[string]string `json:"files"`
	}
	must(json.Unmarshal(files["manifest.json"], &m))
	var sig struct {
		Algorithm string `json:"algorithm"`
		KeyID     string `json:"key_id"`
		Signature string `json:"signature"`
	}
	must(json.Unmarshal(files["signature.json"], &sig))
	raw, err := os.ReadFile(*public)
	must(err)
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	must(err)
	signature, err := base64.StdEncoding.DecodeString(sig.Signature)
	must(err)
	if sig.Algorithm != "ed25519" || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, files["manifest.json"], signature) {
		must(fmt.Errorf("signature verification failed"))
	}
	for name, hash := range m.Files {
		b, ok := files[name]
		if !ok {
			must(fmt.Errorf("missing declared file %s", name))
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != hash {
			must(fmt.Errorf("hash mismatch %s", name))
		}
	}
	if len(files) != len(m.Files)+2 {
		must(fmt.Errorf("undeclared archive entries"))
	}
	result, _ := json.Marshal(map[string]any{"plugin_id": m.ID, "version": m.Version, "publisher": sig.KeyID, "signature": "passed", "file_hashes": "passed", "files": len(m.Files), "uncompressed_bytes": total})
	fmt.Println(string(result))
}
