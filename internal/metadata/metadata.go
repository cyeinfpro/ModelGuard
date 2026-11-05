// Package metadata provides the release manifest and reference-library defaults.
//
//go:generate python3 ../../scripts/sync-metadata.py
package metadata

import (
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed plugin.json
var pluginJSON []byte

//go:embed defaults.json
var defaultsJSON []byte

type Capability struct {
	ID          string `json:"id"`
	Platform    string `json:"platform"`
	AccountType string `json:"account_type"`
}
type PluginMetadata struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Author       string            `json:"author"`
	Requires     map[string]any    `json:"requires"`
	Capabilities []Capability      `json:"capabilities"`
	UI           map[string]string `json:"ui"`
}
type ModelDefaults struct {
	OAuthModel    string            `json:"oauth_model"`
	OAuthExpected string            `json:"oauth_expected"`
	APIModel      string            `json:"api_model"`
	APIExpected   string            `json:"api_expected"`
	Aliases       map[string]string `json:"model_aliases"`
}

var Plugin PluginMetadata
var Defaults ModelDefaults

func init() {
	if err := json.Unmarshal(pluginJSON, &Plugin); err != nil {
		panic(err)
	}
	if err := json.Unmarshal(defaultsJSON, &Defaults); err != nil {
		panic(err)
	}
	if Plugin.ID == "" || len(Plugin.Capabilities) == 0 || Defaults.OAuthModel == "" || Defaults.APIModel == "" {
		panic("invalid embedded plugin defaults")
	}
}
func Manifest(version string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(pluginJSON, &m); err != nil {
		panic(err)
	}
	m["version"] = version
	return m
}
func CanonicalExpected(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if value, ok := Defaults.Aliases[s]; ok {
		return value
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if value, ok := Defaults.Aliases[s]; ok {
		return value
	}
	return s
}
