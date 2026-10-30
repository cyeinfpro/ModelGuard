// Build a signed, hash-verified s2plugin package. Private keys are never copied
// into either the package or its source distribution.
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
	"os"
	"path/filepath"
	"strings"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func main() {
	keyPath := flag.String("key", "", "Ed25519 seed file outside the repository")
	keyID := flag.String("key-id", "cyeinfpro-account-guard-v1", "publisher id")
	gen := flag.Bool("generate-key", false, "create private key if absent")
	out := flag.String("out", "dist/ModelGuard-0.2.1.s2plugin", "output")
	flag.Parse()
	if *keyPath == "" {
		must(fmt.Errorf("-key is required"))
	}
	if *gen {
		must(os.MkdirAll(filepath.Dir(*keyPath), 0700))
		f, err := os.OpenFile(*keyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		must(err)
		seed := make([]byte, 32)
		_, err = rand.Read(seed)
		must(err)
		_, err = f.Write(seed)
		must(err)
		must(f.Close())
	}
	seed, err := os.ReadFile(*keyPath)
	must(err)
	if len(seed) != 32 {
		must(fmt.Errorf("invalid signing seed"))
	}
	private := ed25519.NewKeyFromSeed(seed)
	pub := private.Public().(ed25519.PublicKey)
	rawVersion, err := os.ReadFile("VERSION")
	must(err)
	version := strings.TrimSpace(string(rawVersion))
	files := map[string]string{}
	data := map[string][]byte{}
	for _, root := range []string{"ui", "licenses"} {
		must(filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
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
	for _, name := range []string{"LICENSE", "COPYING", "README.md", "THIRD_PARTY_NOTICES.md"} {
		b, err := os.ReadFile(name)
		must(err)
		data[name] = b
	}
	for name, b := range data {
		h := sha256.Sum256(b)
		files[name] = hex.EncodeToString(h[:])
	}
	manifest := map[string]any{"schema_version": 1, "id": "local.sub2api.account-guard", "name": "模型哨兵", "version": version, "description": "OAuth/API 账号并行检测、失败重试、首轮指纹不符暂停、两轮通过恢复。", "author": "cyeinfpro", "requires": map[string]any{"sub2api": ">=0.2.19 <0.3.0", "recommended_sub2api_version": "0.2.19", "plugin_protocol": 1, "transport_api": 1, "ui_bridge": 1}, "capabilities": []any{map[string]any{"id": "openai.oauth.outbound_transport.v1", "platform": "openai", "account_type": "oauth"}}, "runtimes": map[string]any{"linux-amd64": map[string]string{"path": "runtimes/linux-amd64/account-guard"}, "linux-arm64": map[string]string{"path": "runtimes/linux-arm64/account-guard"}}, "ui": map[string]string{"entrypoint": "ui/index.html"}, "files": files}
	mb, err := json.MarshalIndent(manifest, "", "  ")
	must(err)
	sig := ed25519.Sign(private, mb)
	sb, _ := json.Marshal(map[string]string{"algorithm": "ed25519", "key_id": *keyID, "signature": base64.StdEncoding.EncodeToString(sig)})
	if !ed25519.Verify(pub, mb, sig) {
		must(fmt.Errorf("signature self verification failed"))
	}
	data["manifest.json"] = mb
	data["signature.json"] = sb
	must(os.MkdirAll(filepath.Dir(*out), 0755))
	f, err := os.Create(*out)
	must(err)
	z := zip.NewWriter(f)
	// Stable file order avoids gratuitous archive differences across rebuilds.
	names := make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	sortStrings(names)
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
	must(os.WriteFile(filepath.Join(filepath.Dir(*out), "publisher-public-key.txt"), []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0644))
	must(os.WriteFile(filepath.Join(filepath.Dir(*out), "manifest.json"), mb, 0644))
	fmt.Println("Created", *out, "with verified Ed25519 signature; publisher", *keyID)
}
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
