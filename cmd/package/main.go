// Build a hash-declared s2plugin package, optionally signed with an explicit key.
// Private keys are never copied into the package or its source distribution.
package main

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/cyeinfpro/ModelGuard/internal/metadata"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func main() {
	keyPath := flag.String("key", "", "optional Ed25519 seed file; omit to build unsigned")
	keyID := flag.String("key-id", "cyeinfpro-account-guard-v1", "publisher id")
	gen := flag.Bool("generate-key", false, "create private key if absent")
	out := flag.String("out", "", "output; defaults to dist/ModelGuard-<VERSION>.s2plugin")
	flag.Parse()
	keySpecified := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "key" {
			keySpecified = true
		}
	})
	if (*gen || keySpecified) && strings.TrimSpace(*keyPath) == "" {
		must(fmt.Errorf("-key must name a seed file when signing or generating a key"))
	}
	var private ed25519.PrivateKey
	var pub ed25519.PublicKey
	if *keyPath != "" {
		if strings.TrimSpace(*keyID) == "" {
			must(fmt.Errorf("signing requires a non-empty publisher id"))
		}
		if *gen {
			must(os.MkdirAll(filepath.Dir(*keyPath), 0700))
			seed := make([]byte, ed25519.SeedSize)
			_, err := rand.Read(seed)
			must(err)
			f, err := os.OpenFile(*keyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			must(err)
			_, err = f.Write(seed)
			must(err)
			must(f.Close())
		}
		seed, err := os.ReadFile(*keyPath)
		must(err)
		if len(seed) != ed25519.SeedSize {
			must(fmt.Errorf("invalid signing seed"))
		}
		private = ed25519.NewKeyFromSeed(seed)
		pub = private.Public().(ed25519.PublicKey)
	}
	rawVersion, err := os.ReadFile("VERSION")
	must(err)
	version := strings.TrimSpace(string(rawVersion))
	if *out == "" {
		*out = fmt.Sprintf("dist/ModelGuard-%s.s2plugin", version)
	}
	files := map[string]string{}
	data := map[string][]byte{}
	for _, root := range []string{"ui", "licenses"} {
		must(filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && d.Name() != ".DS_Store" && !strings.HasSuffix(d.Name(), ".pyc") {
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				data[filepath.ToSlash(path)] = b
			}
			return nil
		}))
	}
	for _, arch := range []string{"amd64", "arm64"} {
		b, err := os.ReadFile("build/guard-linux-" + arch)
		must(err)
		data["runtimes/linux-"+arch+"/account-guard"] = b
	}
	for _, name := range []string{"LICENSE", "COPYING", "README.md", "THIRD_PARTY_NOTICES.md", "docs/API-BRIDGE.md", "scripts/api-credential-bridge.py", "scripts/api-bridge-readonly.sql", "scripts/api-bridge.Dockerfile", "scripts/modelguard-api-bridge.service"} {
		b, err := os.ReadFile(name)
		must(err)
		data[name] = b
	}
	for name, b := range data {
		h := sha256.Sum256(b)
		files[name] = hex.EncodeToString(h[:])
	}
	manifest := metadata.Manifest(version)
	manifest["runtimes"] = map[string]any{"linux-amd64": map[string]string{"path": "runtimes/linux-amd64/account-guard"}, "linux-arm64": map[string]string{"path": "runtimes/linux-arm64/account-guard"}}
	manifest["files"] = files
	mb, err := json.MarshalIndent(manifest, "", "  ")
	must(err)
	data["manifest.json"] = mb
	if private != nil {
		sig := ed25519.Sign(private, mb)
		sb, err := json.Marshal(map[string]string{"algorithm": "ed25519", "key_id": *keyID, "signature": base64.StdEncoding.EncodeToString(sig)})
		must(err)
		if !ed25519.Verify(pub, mb, sig) {
			must(fmt.Errorf("signature self verification failed"))
		}
		data["signature.json"] = sb
	}
	must(os.MkdirAll(filepath.Dir(*out), 0755))
	f, err := os.Create(*out)
	must(err)
	z := zip.NewWriter(f)
	// Stable file order avoids gratuitous archive differences across rebuilds.
	names := make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0644)
		if strings.HasPrefix(name, "runtimes/") {
			h.SetMode(0755)
		}
		w, err := z.CreateHeader(h)
		must(err)
		_, err = w.Write(data[name])
		must(err)
	}
	must(z.Close())
	must(f.Close())
	if pub != nil {
		must(os.WriteFile(filepath.Join(filepath.Dir(*out), "publisher-public-key.txt"), []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0644))
	}
	must(os.WriteFile(filepath.Join(filepath.Dir(*out), "manifest.json"), mb, 0644))
	if private == nil {
		fmt.Println("Created", *out, "unsigned; SHA-256 file hashes recorded in manifest (no publisher signature)")
	} else {
		fmt.Println("Created", *out, "with verified Ed25519 signature; publisher", *keyID)
	}
}
