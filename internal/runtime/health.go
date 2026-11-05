package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/cyeinfpro/ModelGuard/internal/guard"
	"github.com/cyeinfpro/ModelGuard/internal/metadata"
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
)

// Large immutable sections stay encoded across Health calls. The engine owns
// its snapshot cache; discovery owns the account list cache. No float64 maps
// are created from either JSON document just to add runtime status fields.
type healthStatus struct {
	StatusVersion            int                               `json:"status_version"`
	ObservedAt               time.Time                         `json:"observed_at"`
	Connection               ConnectionStatus                  `json:"connection"`
	Readiness                ReadinessStatus                   `json:"readiness"`
	RuntimeError             string                            `json:"runtime_error"`
	ManualPending            bool                              `json:"manual_pending"`
	ManualRequestID          string                            `json:"manual_request_id"`
	StopPending              bool                              `json:"stop_pending"`
	StopRequestID            string                            `json:"stop_request_id"`
	ManualBusy               bool                              `json:"manual_busy"`
	ManualQueued             bool                              `json:"manual_queued"`
	BatchBusy                bool                              `json:"batch_busy"`
	Enabled                  *bool                             `json:"enabled,omitempty"`
	AutoPause                *bool                             `json:"auto_pause,omitempty"`
	AutoRecover              *bool                             `json:"auto_recover,omitempty"`
	HostIdentityAvailable    bool                              `json:"host_identity_available"`
	APIBridgeReady           bool                              `json:"api_bridge_ready"`
	APIBridgeState           string                            `json:"api_bridge_state"`
	APIBridgeMessage         string                            `json:"api_bridge_message"`
	DetectorVersion          string                            `json:"detector_version,omitempty"`
	StatusCatalog            map[guard.Status]guard.StatusInfo `json:"status_catalog,omitempty"`
	TargetCount              int                               `json:"target_count"`
	Targets                  []guard.Target                    `json:"targets"`
	ConfiguredTargets        []guard.Target                    `json:"configured_targets"`
	GroupIDs                 []int64                           `json:"group_ids"`
	GroupSyncPendingIDs      []int64                           `json:"group_sync_pending_ids"`
	GroupSyncError           string                            `json:"group_sync_error"`
	MonitoringState          string                            `json:"monitoring_state"`
	AccountsRevision         string                            `json:"accounts_revision"`
	AccountsError            string                            `json:"accounts_error"`
	AccountsLoading          bool                              `json:"accounts_loading"`
	AccountsUpdatedAt        time.Time                         `json:"accounts_updated_at"`
	AccountsCheckedAt        time.Time                         `json:"accounts_checked_at"`
	DiscoveryIntervalSeconds int64                             `json:"discovery_interval_seconds"`
	DiscoverySuspended       bool                              `json:"discovery_suspended"`
	Defaults                 any                               `json:"defaults"`
	Forward                  ForwardMetrics                    `json:"forward"`
}

func accountEncoding(accounts []guard.AccountOption) (json.RawMessage, string, map[int64]guard.AccountOption) {
	if accounts == nil {
		accounts = []guard.AccountOption{}
	}
	raw, _ := json.Marshal(accounts)
	sum := sha256.Sum256(raw)
	index := make(map[int64]guard.AccountOption, len(accounts))
	for _, a := range accounts {
		index[a.ID] = a
	}
	return raw, hex.EncodeToString(sum[:]), index
}

func (p *Plugin) cacheAccountsLocked(accounts []guard.AccountOption) {
	if accounts == nil {
		accounts = []guard.AccountOption{}
	}
	p.accounts = accounts
	p.accountsEncoded, p.accountsRevision, p.accountsIndex = accountEncoding(accounts)
	p.accountsEncodedCount = len(accounts)
}

func (p *Plugin) Health(ctx context.Context, _ *v1.HealthRequest) (*v1.HealthResponse, error) {
	p.mu.RLock()
	e, cfg, host := p.engine, p.config, p.host != nil
	accounts, accountsRaw, revision, index := p.accounts, p.accountsEncoded, p.accountsRevision, p.accountsIndex
	encodedCount, modelsRaw := p.accountsEncodedCount, p.modelsEncoded
	listError, loading, updated, checked := p.accountsError, p.accountsLoading, p.accountsUpdated, p.accountsChecked
	pending, pendingStop, runtimeError := p.pendingManual, p.pendingStop, p.runtimeError
	groupPending, groupError := append([]int64{}, p.groupSyncPending...), p.groupSyncError
	interval, suspended := p.discoveryInterval, p.discoverySuspended
	p.mu.RUnlock()
	if accountsRaw == nil || encodedCount != len(accounts) {
		accountsRaw, revision, index = accountEncoding(accounts)
	}
	if modelsRaw == nil {
		modelsRaw, _ = json.Marshal(p.detector.Models())
	}
	configuredTargets := cfg.Targets
	snapshot := json.RawMessage("{}")
	if e != nil {
		cfg.Targets = e.ManagedTargets()
		snapshot = json.RawMessage(e.Snapshot())
	}
	now := time.Now().UTC()
	if interval <= 0 {
		interval = baseDiscoveryInterval(cfg)
	}
	connection := connectionStatusForInterval(cfg, len(accounts), listError, loading, checked, updated, now, interval)
	bridge := p.bridgeStatus(ctx, cfg.DataDir)
	count := 0
	for _, t := range cfg.Targets {
		if t.Enabled {
			count++
		}
	}
	mode := "disabled"
	if cfg.Enabled {
		mode = "running"
		if count == 0 {
			mode = "waiting_targets"
		}
		if listError != "" {
			mode = "connection_error"
		}
		if loading && updated.IsZero() && listError == "" {
			mode = "loading_accounts"
		}
		if runtimeError != "" {
			mode = "runtime_error"
		}
	}
	status := healthStatus{
		StatusVersion: 3, ObservedAt: now, Connection: connection,
		Readiness:    readinessStatusWithIndex(cfg, index, connection, host, bridge, runtimeError),
		RuntimeError: runtimeError, ManualPending: pending != "", ManualRequestID: cfg.ManualRequestID,
		StopPending: pendingStop != "", StopRequestID: cfg.StopRequestID,
		ManualBusy: e != nil && e.ManualBusy.Load(), ManualQueued: e != nil && e.ManualQueued.Load(), BatchBusy: e != nil && e.BatchBusy.Load(),
		HostIdentityAvailable: host,
		APIBridgeReady:        bridge.State == "installed", APIBridgeState: bridge.State, APIBridgeMessage: bridge.Message,
		TargetCount: count, Targets: cfg.Targets, ConfiguredTargets: configuredTargets,
		GroupIDs: cfg.GroupIDs, GroupSyncPendingIDs: groupPending, GroupSyncError: groupError, MonitoringState: mode,
		AccountsRevision: revision, AccountsError: listError, AccountsLoading: loading, AccountsUpdatedAt: updated, AccountsCheckedAt: checked,
		DiscoveryIntervalSeconds: int64(interval / time.Second), DiscoverySuspended: suspended, Defaults: metadata.Defaults, Forward: p.forwardStats.snapshot(),
	}
	if e == nil {
		status.Enabled, status.AutoPause, status.AutoRecover = &cfg.Enabled, &cfg.AutoPause, &cfg.AutoRecover
		status.DetectorVersion = p.detector.Version()
		status.StatusCatalog = guard.StatusCatalog()
	}
	small, _ := json.Marshal(status)
	// Each fragment is produced by json.Marshal or the engine's own serializer.
	// Splicing preserves integer IDs and avoids re-validating the account payload.
	raw := mergeJSONObject(snapshot, small)
	raw = appendJSONField(raw, "accounts", accountsRaw)
	if e == nil {
		raw = appendJSONField(raw, "models", modelsRaw)
	}
	message := "ModelGuard runtime ready"
	if runtimeError != "" {
		message = runtimeError
	}
	return &v1.HealthResponse{Healthy: runtimeError == "", Message: message, StatusJson: string(raw)}, nil
}

func mergeJSONObject(base, extra []byte) []byte {
	base, extra = bytes.TrimSpace(base), bytes.TrimSpace(extra)
	if len(base) < 2 {
		base = []byte("{}")
	}
	if len(extra) < 2 {
		extra = []byte("{}")
	}
	out := make([]byte, 0, len(base)+len(extra))
	out = append(out, base[:len(base)-1]...)
	if len(base) > 2 && len(extra) > 2 {
		out = append(out, ',')
	}
	out = append(out, extra[1:]...)
	return out
}

func appendJSONField(base []byte, key string, value json.RawMessage) []byte {
	field, _ := json.Marshal(key)
	base = bytes.TrimSpace(base)
	out := make([]byte, 0, len(base)+len(field)+len(value)+3)
	out = append(out, base[:len(base)-1]...)
	if len(base) > 2 {
		out = append(out, ',')
	}
	out = append(out, field...)
	out = append(out, ':')
	out = append(out, value...)
	return append(out, '}')
}

func (p *Plugin) testResponse(start time.Time, success bool, message string, diagnostic map[string]any) *v1.TestConfigResponse {
	h, _ := p.Health(context.Background(), &v1.HealthRequest{})
	diagnostic["checked_at"] = time.Now().UTC()
	raw, _ := json.Marshal(diagnostic)
	status := appendJSONField([]byte(h.StatusJson), "diagnostic", raw)
	if diagnostic["accepted"] == true {
		kind, _ := diagnostic["kind"].(string)
		if kind == "manual" || kind == "stop" {
			state := "queued"
			if kind == "stop" {
				state = "stopped"
			}
			ack, _ := json.Marshal(struct {
				ID       any    `json:"id"`
				Accepted bool   `json:"accepted"`
				State    string `json:"state"`
			}{diagnostic["request_id"], true, state})
			status = appendJSONField(status, kind+"_request", ack)
		}
	}
	return &v1.TestConfigResponse{Success: success, Message: message, LatencyMs: time.Since(start).Milliseconds(), StatusJson: string(status)}
}
