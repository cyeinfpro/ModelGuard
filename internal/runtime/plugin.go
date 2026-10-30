package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"github.com/cyeinfpro/ModelGuard/internal/guard"
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

const ID = "local.sub2api.account-guard"
const Capability = "openai.oauth.outbound_transport.v1"

var Version = "0.3.0"

type Plugin struct {
	v1.UnimplementedTransportPluginServer
	mu              sync.RWMutex
	applyMu         sync.Mutex
	detector        *fingerprint.Detector
	broker          *hcplugin.GRPCBroker
	host            v1.HostServiceClient
	conn            *grpc.ClientConn
	config          guard.Config
	engine          *guard.Engine
	cancel          context.CancelFunc
	done            chan struct{}
	pool            guard.ClientPool
	accounts        []guard.AccountOption
	accountsError   string
	accountsLoading bool
	accountsUpdated time.Time
	accountsChecked time.Time
	accountsEpoch   uint64
	accountsRequest uint64
	pendingManual   string
	runtimeError    string
}

func New() (*Plugin, error) {
	d, err := fingerprint.Load()
	if err != nil {
		return nil, err
	}
	return &Plugin{detector: d, config: guard.DefaultConfig()}, nil
}
func (p *Plugin) SetHostBroker(b *hcplugin.GRPCBroker) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.broker = b
}
func (p *Plugin) InitHostServices(ctx context.Context, r *v1.InitHostServicesRequest) (*v1.InitHostServicesResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broker == nil || r.HostServiceApiVersion < 1 {
		return &v1.InitHostServicesResponse{Message: "宿主不提供账号身份接口"}, nil
	}
	conn, err := p.broker.Dial(r.HostServiceId)
	if err != nil {
		return &v1.InitHostServicesResponse{Message: "宿主服务连接失败"}, nil
	}
	if p.conn != nil {
		p.conn.Close()
	}
	p.conn = conn
	p.host = v1.NewHostServiceClient(conn)
	return &v1.InitHostServicesResponse{Ready: true}, nil
}
func (p *Plugin) resolve(ctx context.Context, id int64) (guard.Identity, error) {
	p.mu.RLock()
	h := p.host
	p.mu.RUnlock()
	if h == nil {
		return guard.Identity{}, errors.New("宿主缺少账号身份接口")
	}
	r, err := h.ResolveOutboundIdentity(ctx, &v1.ResolveOutboundIdentityRequest{AccountId: id})
	if err != nil || r == nil || r.AccountId != id || r.Token == "" {
		return guard.Identity{}, errors.New("账号出站身份不可用")
	}
	headers := http.Header{}
	for k, v := range r.Headers {
		if v == nil {
			continue
		}
		headers[k] = append([]string(nil), v.Values...)
	}
	return guard.Identity{Token: r.Token, Proxy: r.ProxyUrl, Headers: headers}, nil
}
func (p *Plugin) GetInfo(context.Context, *v1.GetInfoRequest) (*v1.GetInfoResponse, error) {
	return &v1.GetInfoResponse{PluginId: ID, PluginVersion: Version, ProtocolVersion: 1, TransportApiVersion: 1, Capabilities: []string{Capability}}, nil
}
func (p *Plugin) Health(context.Context, *v1.HealthRequest) (*v1.HealthResponse, error) {
	p.mu.RLock()
	e, cfg, host := p.engine, p.config, p.host != nil
	accounts := append([]guard.AccountOption{}, p.accounts...)
	listError, loading, updated, checked := p.accountsError, p.accountsLoading, p.accountsUpdated, p.accountsChecked
	pending, runtimeError := p.pendingManual, p.runtimeError
	p.mu.RUnlock()
	now := time.Now().UTC()
	connection := connectionStatus(cfg, len(accounts), listError, loading, checked, updated, now)
	bridge := inspectBridge(cfg.DataDir)
	status := map[string]any{}
	if e != nil {
		_ = json.Unmarshal([]byte(e.Snapshot()), &status)
	}
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
	status["status_version"] = 2
	status["observed_at"] = now
	status["connection"] = connection
	status["readiness"] = readinessStatus(cfg, accounts, connection, host, bridge, runtimeError)
	status["runtime_error"] = runtimeError
	status["manual_pending"] = pending != ""
	status["manual_request_id"] = cfg.ManualRequestID
	status["manual_busy"] = e != nil && e.ManualBusy.Load()
	status["manual_queued"] = e != nil && e.ManualQueued.Load()
	status["batch_busy"] = e != nil && e.BatchBusy.Load()
	status["enabled"] = cfg.Enabled
	status["auto_pause"] = cfg.AutoPause
	status["auto_recover"] = cfg.AutoRecover
	status["host_identity_available"] = host
	status["api_bridge_ready"] = bridge.State == "installed"
	status["api_bridge_state"] = bridge.State
	status["api_bridge_message"] = bridge.Message
	status["models"] = p.detector.Models()
	status["detector_version"] = p.detector.Version()
	status["target_count"] = count
	status["targets"] = cfg.Targets
	status["monitoring_state"] = mode
	status["accounts"] = accounts
	status["accounts_error"] = listError
	status["accounts_loading"] = loading
	status["accounts_updated_at"] = updated
	status["accounts_checked_at"] = checked
	raw, _ := json.Marshal(status)
	message := "ModelGuard runtime ready"
	if runtimeError != "" {
		message = runtimeError
	}
	return &v1.HealthResponse{Healthy: runtimeError == "", Message: message, StatusJson: string(raw)}, nil
}
func (p *Plugin) refreshAccounts(ctx context.Context, c guard.Config) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Bound the entire paginated discovery, not just each individual request.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	p.mu.Lock()
	epoch := p.accountsEpoch
	p.accountsRequest++
	request := p.accountsRequest
	if p.config.AdminURL == c.AdminURL && p.config.AdminKey == c.AdminKey {
		p.accountsLoading = true
	}
	p.mu.Unlock()
	accounts, err := guard.NewAdmin(c).ListAccounts(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.config.AdminURL == c.AdminURL && p.config.AdminKey == c.AdminKey && p.accountsEpoch == epoch && p.accountsRequest == request {
		p.accountsLoading = false
		// Shutdown and replaced configurations are not connectivity failures.
		// An old response must never erase a newer account list or error.
		if errors.Is(ctx.Err(), context.Canceled) {
			return len(accounts), ctx.Err()
		}
		p.accountsChecked = time.Now().UTC()
		if err != nil {
			p.accountsError = err.Error()
		} else {
			p.accounts = accounts
			p.accountsError = ""
			p.accountsUpdated = time.Now().UTC()
		}
	}
	return len(accounts), err
}
func (p *Plugin) discoveryLoop(ctx context.Context, c guard.Config) {
	if strings.TrimSpace(c.AdminKey) == "" {
		<-ctx.Done()
		return
	}
	for {
		_, _ = p.refreshAccounts(ctx, c)
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (p *Plugin) ValidateConfig(ctx context.Context, r *v1.ValidateConfigRequest) (*v1.ValidateConfigResponse, error) {
	c, err := guard.ParseConfig(r.ConfigJson, p.detector)
	if err != nil {
		return &v1.ValidateConfigResponse{Message: err.Error()}, nil
	}
	raw, _ := json.Marshal(c)
	return &v1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: raw}, nil
}
func (p *Plugin) ApplyConfig(ctx context.Context, r *v1.ApplyConfigRequest) (*v1.ApplyConfigResponse, error) {
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	c, err := guard.ParseConfig(r.ConfigJson, p.detector)
	if err != nil {
		return &v1.ApplyConfigResponse{Message: err.Error()}, nil
	}
	p.mu.RLock()
	old := p.config
	oldPending := p.pendingManual
	engine := p.engine
	cancel := p.cancel
	done := p.done
	p.mu.RUnlock()
	if done != nil && reflect.DeepEqual(old, c) {
		return &v1.ApplyConfigResponse{Applied: true, Message: "配置未变更"}, nil
	}
	oldBase, newBase := old, c
	oldBase.ManualRequestID = ""
	newBase.ManualRequestID = ""
	if done != nil && reflect.DeepEqual(oldBase, newBase) {
		if c.ManualRequestID != "" && (engine == nil || engine.ManualBusy.Load()) {
			return &v1.ApplyConfigResponse{Message: "请先保存检测账号，或等待当前检测完成"}, nil
		}
		p.mu.Lock()
		p.config = c
		p.pendingManual = c.ManualRequestID
		p.mu.Unlock()
		return &v1.ApplyConfigResponse{Applied: true, Message: "检测请求已保存"}, nil
	}
	if engine != nil && !engine.TrySuspend() {
		return &v1.ApplyConfigResponse{Message: "检测执行中，编辑内容已保留；请等待本轮完成后保存"}, nil
	}
	if cancel != nil {
		cancel()
		<-done
	}
	// Keep config rollback local: if the new state directory cannot be opened,
	// reopen the previous one and restart its worker before returning failure.
	e, err := p.start(c)
	if err != nil {
		_, restore := p.start(old)
		if restore != nil {
			p.mu.Lock()
			p.engine, p.cancel, p.done = nil, nil, nil
			p.runtimeError = "配置失败且旧监测无法重启；请检查状态目录"
			p.mu.Unlock()
			return &v1.ApplyConfigResponse{Message: "配置失败且旧监测无法重启；请检查状态目录"}, nil
		}
		p.mu.Lock()
		p.pendingManual = oldPending
		p.mu.Unlock()
		return &v1.ApplyConfigResponse{Message: err.Error()}, nil
	}
	// A new manual command saved WITH configuration changes must survive start.
	// Initial/temporary runtime initialization must never replay a stored nonce.
	if done != nil && e != nil && c.ManualRequestID != "" && c.ManualRequestID != old.ManualRequestID {
		p.mu.Lock()
		p.pendingManual = c.ManualRequestID
		p.mu.Unlock()
	}
	message := "配置已保存；定时检测已关闭，可手动检测"
	if c.Enabled {
		message = "配置已保存；定时检测将在 60 秒后开始"
	} else if len(c.Targets) == 0 {
		message = "连接信息已保存，请加载账号"
	}
	return &v1.ApplyConfigResponse{Applied: true, Message: message}, nil
}
func (p *Plugin) start(c guard.Config) (*guard.Engine, error) {
	var e *guard.Engine
	// Keep existing ownership/history visible even after the last target is
	// deselected. An unconfigured fresh installation need not create a store.
	_, stateErr := os.Stat(filepath.Join(c.DataDir, "state.json"))
	if c.Enabled || len(c.Targets) > 0 || !os.IsNotExist(stateErr) {
		admin := guard.NewAdmin(c)
		admin.Enrich = func(ctx context.Context, a *guard.Account) error {
			if a.Type == "apikey" {
				full, err := guard.ReadAPIAccount(ctx, c, a.ID)
				if err != nil {
					return err
				}
				if full.Status != a.Status || full.Schedulable != a.Schedulable {
					return errors.New("API 账号状态已变化，请重试")
				}
				*a = full
				return nil
			}
			return p.enrichAccount(ctx, a)
		}
		probe := &guard.Prober{Resolve: p.resolve, Pool: &p.pool}
		var err error
		e, err = guard.NewEngine(c, p.detector, admin, probe)
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.mu.Lock()
	sameConnection := p.config.AdminURL == c.AdminURL && p.config.AdminKey == c.AdminKey
	p.engine = e
	p.pendingManual = ""
	p.config = c
	p.cancel = cancel
	p.done = done
	p.runtimeError = ""
	p.accountsEpoch++
	p.accountsLoading = strings.TrimSpace(c.AdminKey) != ""
	if !sameConnection {
		p.accounts = []guard.AccountOption{}
		p.accountsError = ""
		p.accountsUpdated = time.Time{}
		p.accountsChecked = time.Time{}
	}
	p.mu.Unlock()
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		if e != nil {
			wg.Add(1)
			go func() { defer wg.Done(); e.Run(ctx) }()
		}
		p.discoveryLoop(ctx, c)
		wg.Wait()
	}()
	return e, nil
}
func (p *Plugin) TestConfig(ctx context.Context, r *v1.TestConfigRequest) (*v1.TestConfigResponse, error) {
	start := time.Now()
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	c, err := guard.ParseConfig(r.ConfigJson, p.detector)
	if err != nil {
		return p.testResponse(start, false, err.Error(), map[string]any{"kind": "connection", "code": "config_invalid", "config_applied": false}), nil
	}
	p.mu.RLock()
	engine, request, committed, applied := p.engine, p.pendingManual, p.config, p.done != nil
	p.mu.RUnlock()
	configMatches := reflect.DeepEqual(c, committed)
	if err := ctx.Err(); err != nil {
		return p.testResponse(start, false, "请求已取消，请重新操作", map[string]any{"kind": "connection", "code": "request_cancelled", "config_applied": applied && configMatches}), nil
	}
	// Only a command explicitly saved in this process may trigger a run.
	// Validate before consuming it; mismatched diagnostics and transient queue
	// errors must not silently lose a user's pending click.
	if request != "" {
		meta := map[string]any{"kind": "manual", "config_applied": applied && configMatches, "request_id": request}
		if c.ManualRequestID != request || !configMatches {
			meta["code"] = "config_mismatch"
			return p.testResponse(start, false, "检测请求与已保存配置不一致，请重新加载配置", meta), nil
		}
		if engine == nil {
			meta["code"] = "runtime_unavailable"
			return p.testResponse(start, false, "监测服务尚未就绪，请先选择账号并保存", meta), nil
		}
		if err := engine.RequestManual(request); err != nil {
			meta["code"] = "manual_not_accepted"
			return p.testResponse(start, false, err.Error(), meta), nil
		}
		p.mu.Lock()
		p.pendingManual = ""
		p.mu.Unlock()
		meta["code"] = "manual_queued"
		meta["accepted"] = true
		return p.testResponse(start, true, "已加入检测队列，将按并发上限逐个检测已选账号", meta), nil
	}
	// v1's iframe bridge tests saved configuration. Reject a stale request
	// rather than presenting its result as the current runtime's connection.
	if applied && !configMatches {
		return p.testResponse(start, false, "配置已发生变化，请重新加载后再检测连接", map[string]any{"kind": "connection", "code": "config_mismatch", "config_applied": false}), nil
	}
	// Check authentication and pagination even when there are no targets.
	n, err := p.refreshAccounts(ctx, c)
	meta := map[string]any{"kind": "connection", "config_applied": applied && configMatches, "account_count": n}
	if err != nil {
		meta["code"] = connectionErrorCode(err.Error())
		return p.testResponse(start, false, err.Error(), meta), nil
	}
	selected := 0
	for _, t := range c.Targets {
		if t.Enabled {
			selected++
		}
	}
	message := fmt.Sprintf("管理端已连接，读取到 %d 个授权账号；已选择 %d 个检测目标", n, selected)
	meta["code"] = "connection_ready"
	return p.testResponse(start, true, message, meta), nil
}

func (p *Plugin) testResponse(start time.Time, success bool, message string, diagnostic map[string]any) *v1.TestConfigResponse {
	h, _ := p.Health(context.Background(), &v1.HealthRequest{})
	status := map[string]any{}
	_ = json.Unmarshal([]byte(h.StatusJson), &status)
	diagnostic["checked_at"] = time.Now().UTC()
	status["diagnostic"] = diagnostic
	if diagnostic["kind"] == "manual" && diagnostic["accepted"] == true {
		status["manual_request"] = map[string]any{"id": diagnostic["request_id"], "accepted": true, "state": "queued"}
	}
	raw, _ := json.Marshal(status)
	return &v1.TestConfigResponse{Success: success, Message: message, LatencyMs: time.Since(start).Milliseconds(), StatusJson: string(raw)}
}
func (p *Plugin) Close() {
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	p.mu.RLock()
	cancel, done := p.cancel, p.done
	p.mu.RUnlock()
	if cancel != nil {
		cancel()
		<-done
	}
	p.pool.Close()
	p.mu.Lock()
	if p.conn != nil {
		p.conn.Close()
	}
	p.cancel = nil
	p.done = nil
	p.engine = nil
	p.host = nil
	p.conn = nil
	p.pendingManual = ""
	p.accountsEpoch++
	p.accountsLoading = false
	p.mu.Unlock()
}
