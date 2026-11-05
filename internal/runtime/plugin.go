package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"github.com/cyeinfpro/ModelGuard/internal/guard"
	"github.com/cyeinfpro/ModelGuard/internal/metadata"
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

var ID = metadata.Plugin.ID
var Capability = metadata.Plugin.Capabilities[0].ID
var Version = metadata.Version

type Plugin struct {
	v1.UnimplementedTransportPluginServer
	mu                   sync.RWMutex
	applyMu              sync.Mutex
	discoveryMu          sync.Mutex
	detector             *fingerprint.Detector
	broker               *hcplugin.GRPCBroker
	host                 v1.HostServiceClient
	conn                 *grpc.ClientConn
	config               guard.Config
	engine               *guard.Engine
	cancel               context.CancelFunc
	done                 chan struct{}
	pool                 guard.ClientPool
	groupSyncPending     []int64
	groupSyncError       string
	accounts             []guard.AccountOption
	accountsError        string
	accountsLoading      bool
	accountsUpdated      time.Time
	accountsChecked      time.Time
	accountsEpoch        uint64
	accountsRequest      uint64
	pendingManual        string
	runtimeError         string
	pendingStop          string
	modelsEncoded        json.RawMessage
	accountsEncoded      json.RawMessage
	accountsRevision     string
	accountsIndex        map[int64]guard.AccountOption
	accountsEncodedCount int
	discoveryWake        chan struct{}
	discoveryRefresh     bool
	discoveryInterval    time.Duration
	discoverySuspended   bool
	discoveryFailures    int
	metadataMu           sync.Mutex
	metadataCache        map[string]*accountMetadataEntry
	hostEpoch            uint64
	bridgeMu             sync.Mutex
	bridgeDir            string
	bridgeChecked        time.Time
	bridgeCached         bridgeInstallation
	forwardStats         forwardCounters
}

func New() (*Plugin, error) {
	d, err := fingerprint.Load()
	if err != nil {
		return nil, err
	}
	p := &Plugin{detector: d, config: guard.DefaultConfig()}
	p.modelsEncoded, _ = json.Marshal(d.Models())
	p.cacheAccountsLocked(nil)
	return p, nil
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
	p.hostEpoch++
	p.invalidateMetadata()
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
	p.discoveryMu.Lock()
	defer p.discoveryMu.Unlock()
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
		p.discoveryResultLocked(p.config, err)
		base, current := c, p.config
		base.ManualRequestID = ""
		current.ManualRequestID = ""
		base.StopRequestID = ""
		current.StopRequestID = ""
		syncGroups := len(c.GroupIDs) > 0 && reflect.DeepEqual(base, current)
		if err != nil {
			p.accountsError = err.Error()
			if syncGroups {
				engine := p.engine
				p.mu.Unlock()
				if engine != nil {
					engine.SetGroupSyncBlocked(true)
				}
				p.mu.Lock()
				if p.accountsEpoch == epoch && p.engine == engine {
					p.groupSyncError = err.Error()
				}
			}
		} else {
			p.cacheAccountsLocked(accounts)
			p.accountsError = ""
			p.accountsUpdated = time.Now().UTC()
			if syncGroups {
				targets, pending, syncErr := guard.TargetsForGroups(c, accounts)
				engine := p.engine
				if syncErr == nil && engine != nil {
					// Control paths may call back into host identity under p.mu.
					// Never wait for an engine control lock while holding p.mu.
					p.mu.Unlock()
					syncErr = engine.UpdateManagedTargets(targets)
					p.mu.Lock()
				}
				if p.accountsEpoch != epoch || p.engine != engine {
					return len(accounts), err
				}
				if syncErr != nil {
					p.mu.Unlock()
					if engine != nil {
						engine.SetGroupSyncBlocked(true)
					}
					p.mu.Lock()
					if p.accountsEpoch != epoch || p.engine != engine {
						return len(accounts), syncErr
					}
					p.groupSyncError = syncErr.Error()
					return len(accounts), syncErr
				} else {
					p.groupSyncError = ""
					p.groupSyncPending = pending
				}
			}
		}
	}
	return len(accounts), err
}
func (p *Plugin) ValidateConfig(ctx context.Context, r *v1.ValidateConfigRequest) (*v1.ValidateConfigResponse, error) {
	c, err := guard.ParseConfig(r.ConfigJson, p.detector)
	if err != nil {
		return &v1.ValidateConfigResponse{Message: err.Error()}, nil
	}
	raw, _ := json.Marshal(c)
	return &v1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: raw}, nil
}

// Hard fields identify administrative authority and durable state. All other
// settings are applied to the live engine without resetting startup grace.
func sameRuntimeAuthority(a, b guard.Config) bool {
	return a.AdminURL == b.AdminURL && a.AdminKey == b.AdminKey && a.DataDir == b.DataDir
}
func withoutActions(c guard.Config) guard.Config {
	c.ManualRequestID, c.StopRequestID = "", ""
	return c
}
func (p *Plugin) saveCommandsLocked(old, c guard.Config) {
	if c.ManualRequestID == "" {
		p.pendingManual = ""
	} else if c.ManualRequestID != old.ManualRequestID {
		p.pendingManual = c.ManualRequestID
	}
	if c.StopRequestID == "" {
		p.pendingStop = ""
	} else if c.StopRequestID != old.StopRequestID {
		p.pendingStop = c.StopRequestID
		p.pendingManual = ""
	}
}
func (p *Plugin) ApplyConfig(ctx context.Context, r *v1.ApplyConfigRequest) (*v1.ApplyConfigResponse, error) {
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	c, err := guard.ParseConfig(r.ConfigJson, p.detector)
	if err != nil {
		return &v1.ApplyConfigResponse{Message: err.Error()}, nil
	}
	p.mu.RLock()
	old, oldPending, oldStop := p.config, p.pendingManual, p.pendingStop
	engine, cancel, done := p.engine, p.cancel, p.done
	p.mu.RUnlock()
	if done != nil && reflect.DeepEqual(old, c) {
		return &v1.ApplyConfigResponse{Applied: true, Message: "配置未变更"}, nil
	}
	newManual := c.ManualRequestID != "" && c.ManualRequestID != old.ManualRequestID
	newStop := c.StopRequestID != "" && c.StopRequestID != old.StopRequestID
	if done != nil && newManual && !newStop && (engine == nil || engine.ManualBusy.Load()) {
		return &v1.ApplyConfigResponse{Message: "请先保存检测账号，或等待当前检测完成"}, nil
	}
	actionOnly := reflect.DeepEqual(withoutActions(old), withoutActions(c))
	needsEngine := c.Enabled || len(c.Targets) > 0 || len(c.GroupIDs) > 0
	if done != nil && sameRuntimeAuthority(old, c) && (engine != nil || !needsEngine) {
		// Discovery performs network I/O before this lock. Serialize only its
		// commit with a soft config commit, so old membership cannot overwrite
		// newly selected groups after UpdateConfig has accepted them.
		p.discoveryMu.Lock()
		defer p.discoveryMu.Unlock()
		if engine != nil && !actionOnly {
			if err := engine.UpdateConfig(c); err != nil {
				return &v1.ApplyConfigResponse{Message: err.Error()}, nil
			}
		}
		p.mu.Lock()
		p.config = c
		p.saveCommandsLocked(old, c)
		if !actionOnly {
			p.accountsEpoch++
			p.discoveryFailures = 0
			p.discoverySuspended = false
			p.discoveryInterval = baseDiscoveryInterval(c)
			p.wakeDiscoveryLocked(true)
		}
		p.mu.Unlock()
		message := "配置已保存并应用，当前检测继续执行"
		if actionOnly {
			message = "操作请求已保存"
		}
		return &v1.ApplyConfigResponse{Applied: true, Message: message}, nil
	}
	if engine != nil && !engine.TrySuspend() {
		return &v1.ApplyConfigResponse{Message: "管理端或状态目录变更需要检测空闲；请先停止本轮检测再保存"}, nil
	}
	if cancel != nil {
		cancel()
		<-done
	}
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
		p.pendingManual, p.pendingStop = oldPending, oldStop
		p.mu.Unlock()
		return &v1.ApplyConfigResponse{Message: err.Error()}, nil
	}
	// Persisted action IDs are inert on initialization. Only new commands saved
	// in this process can be consumed by the following config.test call.
	if done != nil {
		p.mu.Lock()
		if e != nil && newManual {
			p.pendingManual = c.ManualRequestID
		}
		if newStop {
			p.pendingStop = c.StopRequestID
			p.pendingManual = ""
		}
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
	if c.Enabled || len(c.Targets) > 0 || len(c.GroupIDs) > 0 || !os.IsNotExist(stateErr) {
		admin := guard.NewAdmin(c)
		enrich := func(ctx context.Context, a *guard.Account, fresh bool) error {
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
			if fresh {
				return p.enrichAccountFresh(ctx, a)
			}
			return p.enrichAccount(ctx, a)
		}
		admin.Enrich = func(ctx context.Context, a *guard.Account) error { return enrich(ctx, a, false) }
		admin.EnrichFresh = func(ctx context.Context, a *guard.Account) error { return enrich(ctx, a, true) }
		probe := &guard.Prober{Resolve: p.resolve}
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
	p.pendingStop = ""
	p.config = c
	p.cancel = cancel
	p.done = done
	p.runtimeError = ""
	p.accountsEpoch++
	p.groupSyncPending = nil
	p.groupSyncError = ""
	p.accountsLoading = strings.TrimSpace(c.AdminKey) != ""
	p.discoveryWake = make(chan struct{}, 1)
	p.discoveryRefresh = false
	p.discoveryInterval = baseDiscoveryInterval(c)
	p.discoverySuspended = false
	p.discoveryFailures = 0
	p.invalidateMetadata()
	if !sameConnection {
		p.cacheAccountsLocked(nil)
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
	engine, request, stopRequest, committed, applied := p.engine, p.pendingManual, p.pendingStop, p.config, p.done != nil
	p.mu.RUnlock()
	configMatches := reflect.DeepEqual(c, committed)
	if err := ctx.Err(); err != nil {
		return p.testResponse(start, false, "请求已取消，请重新操作", map[string]any{"kind": "connection", "code": "request_cancelled", "config_applied": applied && configMatches}), nil
	}
	if stopRequest != "" {
		meta := map[string]any{"kind": "stop", "config_applied": applied && configMatches, "request_id": stopRequest}
		if c.StopRequestID != stopRequest || !configMatches {
			meta["code"] = "config_mismatch"
			return p.testResponse(start, false, "停止请求与已保存配置不一致，请重新加载配置", meta), nil
		}
		interrupted := false
		if engine != nil {
			interrupted = engine.StopCurrent()
		}
		p.mu.Lock()
		p.pendingStop = ""
		p.pendingManual = ""
		p.mu.Unlock()
		meta["code"] = "stop_accepted"
		meta["accepted"] = true
		meta["interrupted"] = interrupted
		return p.testResponse(start, true, "本轮检测已停止，已有隔离状态保持；后续按重试时间继续", meta), nil
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
	p.invalidateMetadata()
	n, err := p.refreshAccounts(ctx, c)
	p.mu.Lock()
	p.wakeDiscoveryLocked(false)
	p.mu.Unlock()
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
	p.hostEpoch++
	p.conn = nil
	p.pendingManual = ""
	p.pendingStop = ""
	p.accountsEpoch++
	p.groupSyncPending = nil
	p.groupSyncError = ""
	p.accountsLoading = false
	p.invalidateMetadata()
	p.mu.Unlock()
}
