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
	"sync"
	"time"
)

const ID = "local.sub2api.account-guard"
const Capability = "openai.oauth.outbound_transport.v1"

var Version = "0.2.1"

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
	pendingManual   string
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
	listError, loading, updated := p.accountsError, p.accountsLoading, p.accountsUpdated
	p.mu.RUnlock()
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
		if loading && updated.IsZero() {
			mode = "loading_accounts"
		}
	}
	status["manual_busy"] = e != nil && e.ManualBusy.Load()
	status["manual_queued"] = e != nil && e.ManualQueued.Load()
	status["batch_busy"] = e != nil && e.BatchBusy.Load()
	status["enabled"] = cfg.Enabled
	status["auto_pause"] = cfg.AutoPause
	status["auto_recover"] = cfg.AutoRecover
	status["host_identity_available"] = host
	_, bridgeErr := os.Stat(filepath.Join(cfg.DataDir, "api-bridge.sock"))
	status["api_bridge_ready"] = bridgeErr == nil
	status["models"] = p.detector.Models()
	status["detector_version"] = p.detector.Version()
	status["target_count"] = count
	status["targets"] = cfg.Targets
	status["monitoring_state"] = mode
	status["accounts"] = accounts
	status["accounts_error"] = listError
	status["accounts_loading"] = loading
	status["accounts_updated_at"] = updated
	raw, _ := json.Marshal(status)
	return &v1.HealthResponse{Healthy: true, Message: "Account Guard runtime ready", StatusJson: string(raw)}, nil
}
func (p *Plugin) refreshAccounts(ctx context.Context, c guard.Config) (int, error) {
	accounts, err := guard.NewAdmin(c).ListAccounts(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.config.AdminURL == c.AdminURL && p.config.AdminKey == c.AdminKey {
		p.accountsLoading = false
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
	for {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, _ = p.refreshAccounts(bounded, c)
		cancel()
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
	if engine != nil && (engine.ManualBusy.Load() || engine.BatchBusy.Load()) {
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
			return &v1.ApplyConfigResponse{Message: "配置失败且旧监测无法重启；请检查状态目录"}, nil
		}
		return &v1.ApplyConfigResponse{Message: err.Error()}, nil
	}
	// A new manual command saved WITH configuration changes must survive start.
	// Initial/temporary runtime initialization must never replay a stored nonce.
	if done != nil && e != nil && c.ManualRequestID != "" && c.ManualRequestID != old.ManualRequestID {
		p.mu.Lock()
		p.pendingManual = c.ManualRequestID
		p.mu.Unlock()
	}
	return &v1.ApplyConfigResponse{Applied: true, Message: "配置已保存；定时检测将在 60 秒后开始"}, nil
}
func (p *Plugin) start(c guard.Config) (*guard.Engine, error) {
	var e *guard.Engine
	if c.Enabled || len(c.Targets) > 0 {
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
	p.engine = e
	p.pendingManual = ""
	p.config = c
	p.cancel = cancel
	p.done = done
	p.accounts = []guard.AccountOption{}
	p.accountsError = ""
	p.accountsLoading = true
	p.accountsUpdated = time.Time{}
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
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	p.mu.Lock()
	engine, request := p.engine, p.pendingManual
	// Only an explicit command saved in this running process may trigger a run.
	// Startup/temporary test processes cannot replay persisted commands.
	p.pendingManual = ""
	p.mu.Unlock()
	if request != "" && engine != nil {
		c, err := guard.ParseConfig(r.ConfigJson, p.detector)
		if err != nil || c.ManualRequestID != request {
			return &v1.TestConfigResponse{Message: "检测请求与已保存配置不一致"}, nil
		}
		if err := engine.RequestManual(request); err != nil {
			return &v1.TestConfigResponse{Message: err.Error()}, nil
		}
		h, _ := p.Health(ctx, &v1.HealthRequest{})
		return &v1.TestConfigResponse{Success: true, Message: "检测已开始，正在按并发上限检测已选账号", StatusJson: h.StatusJson}, nil
	}
	start := time.Now()
	c, err := guard.ParseConfig(r.ConfigJson, p.detector)
	if err != nil {
		return &v1.TestConfigResponse{Message: err.Error()}, nil
	}
	// Check authentication and pagination even when there are no targets.
	n, err := p.refreshAccounts(ctx, c)
	if err != nil {
		return &v1.TestConfigResponse{Message: err.Error()}, nil
	}
	selected := 0
	for _, t := range c.Targets {
		if t.Enabled {
			selected++
		}
	}
	message := fmt.Sprintf("已获取 %d 个账号；已选择 %d 个检测目标", n, selected)
	message += "；OAuth 与 API 账号均按指定账号独立检测"
	h, _ := p.Health(ctx, &v1.HealthRequest{})
	return &v1.TestConfigResponse{Success: true, Message: message, LatencyMs: time.Since(start).Milliseconds(), StatusJson: h.StatusJson}, nil
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
	p.mu.Unlock()
}
