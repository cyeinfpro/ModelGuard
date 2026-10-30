package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
	"github.com/hashicorp/go-hclog"
	hcplugin "github.com/hashicorp/go-plugin"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	binary := flag.String("binary", "build/guard-native", "native executable")
	flag.Parse()
	raw, err := os.ReadFile(*binary)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	c := hcplugin.NewClient(&hcplugin.ClientConfig{HandshakeConfig: v1.HandshakeConfig, Plugins: v1.ClientPluginMap(), Cmd: exec.Command(*binary), AllowedProtocols: []hcplugin.Protocol{hcplugin.ProtocolGRPC}, SecureConfig: &hcplugin.SecureConfig{Checksum: sum[:], Hash: sha256.New()}, SkipHostEnv: true, Logger: hclog.NewNullLogger(), SyncStdout: io.Discard, SyncStderr: io.Discard, StartTimeout: 15 * time.Second})
	defer c.Kill()
	client, err := c.Client()
	if err != nil {
		panic(err)
	}
	obj, err := client.Dispense(v1.TransportPluginName)
	if err != nil {
		panic(err)
	}
	api := obj.(*v1.TransportClient)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info, err := api.GetInfo(ctx, &v1.GetInfoRequest{})
	if err != nil {
		panic(err)
	}
	expectedVersion, err := os.ReadFile("VERSION")
	if err != nil {
		panic(err)
	}
	if info.PluginVersion != strings.TrimSpace(string(expectedVersion)) {
		panic("built runtime version does not match VERSION")
	}
	v, err := api.ValidateConfig(ctx, &v1.ValidateConfigRequest{ConfigJson: []byte(`{}`)})
	if err != nil || !v.Valid {
		panic("ValidateConfig failed")
	}
	a, err := api.ApplyConfig(ctx, &v1.ApplyConfigRequest{ConfigJson: v.NormalizedConfigJson})
	if err != nil || !a.Applied {
		panic("ApplyConfig failed")
	}
	h, err := api.Health(ctx, &v1.HealthRequest{})
	if err != nil || !h.Healthy {
		panic("Health failed")
	}
	t, err := api.TestConfig(ctx, &v1.TestConfigRequest{ConfigJson: v.NormalizedConfigJson})
	if err != nil || t.Success || t.Message != "请先保存管理员访问密钥，再加载账号" {
		panic("TestConfig failed")
	}
	b, _ := json.Marshal(map[string]any{"plugin_id": info.PluginId, "version": info.PluginVersion, "sha256": hex.EncodeToString(sum[:]), "handshake": "passed", "config": "passed", "health": "passed", "diagnostic": "expected_not_configured"})
	fmt.Println(string(b))
}
