# ModelGuard v0.4.0 优化落地记录

日期：2026-11-05（北京时间）

审查基线：`eba5599` / v0.3.2

需求来源：[OPTIMIZATION_2026-11-05.md](OPTIMIZATION_2026-11-05.md)

目标版本：**v0.4.0**

本文逐项记录原建议 §2.1–§6.4 的实施、兼容选择与验证证据。当前记录覆盖源码和专项合成验证；最终提交、标签、完整构建、远端 CI、Release 和 VPS 更新回执在文末单独补充，不能从“已实现”推断“已上线”。

## 一、逐项实施矩阵

### 2. 正确性与策略

| 原建议 | 实施结果与行为 | 主要依据 |
| --- | --- | --- |
| §2.1 配置错误先扣预算 | 增加 `PreparedProbe / PreparedSample`：先解析身份、端点、凭据、代理并构造请求，再预留预算。配置错误立即结束本轮。真实 Prober 不再为未通过准备的请求扣数；在实际请求调用前保守预留，不退还结果不明的网络失败，避免并发超额或重启后重复使用额度。 | `internal/guard/probe.go`、`sample.go`、`probe_optimization_test.go` |
| §2.2 错误分级与退避 | 永久性 HTTP 错误不重试；408/409/425/429、5xx 和暂时性传输失败走可重试路径。退避以 5 秒、15 秒为基数，加 0.8–1.2 倍抖动；支持秒数和 HTTP 日期形式的 Retry-After。超过 60 秒的本轮重试等待直接结束本轮，受请求上下文取消约束。仅透出预定义白名单中的 error.code/type，不显示原始错误正文或任意上游字符串。 | `internal/guard/http_errors.go`、`probe.go`、`probe_optimization_test.go` |
| §2.3 不确定与采样失败分开 | 增加 `inconclusive` 状态与计数。完整三份有效样本的模糊评分计为不确定；不足三份、采样中断等计为不可用，不产生暂停或恢复确认。手动统计分开报告通过、不符、不确定、采样失败、跳过；UI 展示前三候选及概率。可选家族匹配默认关闭，结果带 `match_scope`。 | `state.go`、`status.go`、`manual.go`、`engine.go`、`internal/fingerprint/detector.go`、`ui/monitor.js` |
| §2.4 审计与采样日志分离 | 暂停、恢复、归属、归档、配置等审计写入 `audit.jsonl`；采样和 check 活动留在独立内存缓冲。审计不受采样数量挤压。当前按数量保留最近至少 2,000 条审计，不承诺完整 30 天留存；日志按 4,000 条或 8 MiB 轮转到 `audit.jsonl.1`。UI 默认审计，勾选后合并采样。 | `persistence.go`、`state.go`、`ui/monitor.js`、`persistence_optimization_test.go` |
| §2.5 恢复轮次间隔 | 新增 `recovery_spacing_seconds`，默认 **0**，延续原 retry_seconds 节奏；设置正值后，两次有效恢复确认至少相隔指定时间，手动连续点击也不能绕过。保留连续两轮有效通过规则。 | `config.go`、`state.go`、`scheduler_optimization_test.go`、`ui/index.html` |
| §2.6 状态目录与重复分类 | 引入 `Status` 常量和下发的 `status_catalog`，补齐 archived/pending 中文语义。手动与调度统计共用关注分类。旧 suspect/paused_observation 在加载时迁移；旧隔离归属仍通过读取和新样本核对。暂时性管理连接错误、429/5xx 不清零已有恢复证据，也不推进；确认不可见/拒绝访问等错误仍清零。 | `status.go`、`engine.go`、`manual.go`、`pipeline.go`、`engine_targets.go` |
| §2.7 校准绑定校验 | Decode 校验校准 schema/method、有限且合理的温度参数、哈希格式、参考绑定及模型顺序；无效参考库使加载失败，避免长期静默返回不确定。 | `internal/fingerprint/detector.go`、`optimization_test.go` |

### 3. 性能与资源

| 原建议 | 实施结果与行为 | 主要依据 |
| --- | --- | --- |
| §3.1 按需快照 | publish 只更新修订号和错误文本；读取 Snapshot 时复制一致状态，释放 stateMu 后序列化。记录/事件等冷数据按 state_revision 缓存，活动进度单独组成热快照；模型元数据预编码。并发读取共享缓存，进度回调不再整包序列化。 | `engine.go`、`snapshot.go`、`persistence.go` |
| §3.2 分级持久化 | 关键状态、控制意图/回执、手动入队保持同步原子提交。state.json 改为紧凑 JSON，并保留上一版 state.json.prev。采样活动不触发全量写盘。预算用独立小文件按 16 次上限分块持久化；崩溃时最多保守计入 15 次未实际使用额度，正常关闭收敛到实际使用数。暂时性落盘错误每 5 秒重试，成功前不放行新请求。 | `persistence.go`、`engine.go`、`scheduler.go`、`sample.go`、`retry.go` |
| §3.3 Health 编码与修订号 | 用结构体和 RawMessage 拼装引擎、账号、模型片段；避免快照解码成浮点 map 再编码。发现成功后缓存账号 JSON 和内容哈希 `accounts_revision`，UI 按修订号更新账号。**保留宿主 v1 的完整 accounts 返回契约；可选的“摘要轮询/按需账号列表”未启用**，不宣称实现了条件请求或独立列表接口。 | `internal/runtime/health.go`、`ui/monitor.js` |
| §3.4 自适应发现 | 手选模式 120 秒，分组模式 15 秒；显式重新加载立即请求。401/403 暂停自动轮询，配置变更或手动重试可重新检查。其他失败从 10 秒指数退避至 300 秒，服务端更长 Retry-After 作为等待下限。过期判断随实际周期调整；完整分组发现失败继续冻结旧成员关系的采样/恢复。 | `internal/runtime/discovery.go`、`plugin.go`、`status.go`、`discovery_test.go` |
| §3.5 OAuth 元数据缓存 | 按宿主/平台/类型缓存列表，TTL 为 5 秒，复用并发查询结果；管理写入后的复读走强制刷新。宿主连接或配置切换使旧缓存失效。未修改宿主 protobuf 增加 account_ids，保留现有宿主协议。 | `internal/runtime/account_metadata.go`、`account_metadata_test.go`、`internal/guard/admin.go` |
| §3.6 转发与探测分离 | ClientPool 仅用于生产转发：无额外 ResponseHeaderTimeout，由宿主上下文控制；每主机 32 个空闲连接、最多 64 个代理池，按 LRU 淘汰。采样逐份克隆独立传输并禁用长连接。Health 加入请求/进行中/完成数、错误码和 HTTP 状态类别；最近 512 份首包观测计算 p50/p95/p99，不记录内容或凭据。 | `internal/guard/transport.go`、`probe.go`、`internal/runtime/forward.go`、`forward_metrics.go` |
| §3.7 UI 增量渲染 | 建立账号 Map；账号表按修订号更新，记录按 account_id 复用节点，事件按稳定键复用。更新文字/单元格，避免每次状态轮询重建整个列表；保留输入焦点和未保存草稿。 | `ui/accounts.js`、`monitor.js`、`state.js`、`tests/ui.optimization.test.mjs` |

### 4. 调度与可用性

| 原建议 | 实施结果与行为 | 主要依据 |
| --- | --- | --- |
| §4.1 持续调度 | 长期 Run 使用到期堆和共享的 1–8 个执行名额，每个空闲名额立即接收下一账号；慢账号只占自身名额。新到期账号和手动请求可在其他名额继续执行。手动请求可合并到尚在执行的同账号轮次，避免重复付费采样；已完成轮次不会被错误复用。批次投影改为最近一小时统计，保留兼容字段。同步 Tick/runManual 仅保留为有限调用及测试入口。 | `scheduler.go`、`parallel.go`、`manual.go`、`pipeline.go` |
| §4.2 热更新与停止 | `CurrentConfig/UpdateConfig` 在 stateMu 下原子地替换软设置，不重启引擎、不重置 60 秒启动期。采样保留本轮超时，下一次策略决策使用当前开关；目标变化使旧代际结果失效。新增一次性 stop_request_id；StopCurrent 取消当前请求和待执行手动工作，保留已有暂停，后续按重试时间继续。管理 URL/Key/数据目录仍使用空闲挂起和重启流程。 | `config_runtime.go`、`engine.go`、`internal/runtime/plugin.go`、`ui/app.js` |
| §4.3 控制锁收窄 | 移除全局 controlMu 对网络读写的串行阻塞；同账号由 inFlight 排他，网络 I/O 释放 stateMu。写前再次核对目标代际和当前策略，移出/停止会取消对应控制上下文；意图与回执依然同步落盘。运行时软配置与发现结果共用提交锁，避免旧成员快照覆盖新配置；该锁不跨分页网络或硬重启等待。宿主无原子条件写接口，已发出的远端写请求仍存在观察窗口，不能宣称完全消除宿主侧竞态。 | `engine.go`、`engine_targets.go`、`config_runtime.go`、`scheduler_optimization_test.go` |

### 5. 安全与权限

| 原建议 | 实施结果与行为 | 主要依据 |
| --- | --- | --- |
| §5.1 桥接最小权限 | 默认 direct 模式，通过 psycopg 直接连接 PostgreSQL；交付专用只读角色和投影视图 SQL，只投影 OpenAI API Key 独立账号及必需代理字段，不读取整份 credentials。角色无基础表授权，连接设只读和超时，进程可按与插件相同的非 root UID/GID 运行。交付加固 systemd 和非 root 伴随容器方案。带本地令牌的 /healthz 只验证在线，不触碰账号、数据库或模型。旧 docker 模式仅显式兼容，默认不降级；生产角色和服务的实际迁移结果待部署回执。 | `scripts/api-credential-bridge.py`、`api-bridge-readonly.sql`、`api-bridge.Dockerfile`、`modelguard-api-bridge.service`、[API-BRIDGE.md](API-BRIDGE.md) |
| §5.2 回环地址一致性 | Go 桥接读取、运行时就绪判断和 Python 服务统一接受合法回环主机及任意有效端口，包括 localhost、IPv4 回环、IPv6 ::1；拒绝路径、用户信息、查询、跳转和非回环桥接地址。8080 仍是示例默认值，不再是可用性的硬编码门槛。 | `internal/guard/api_bridge.go`、`internal/runtime/status.go`、`scripts/api-credential-bridge.py` |
| §5.3 postMessage 与生命周期 | 优先固定 ancestorOrigins、宿主下发 parent_origin 或 referrer 中的有效父页面源，收信同时核验 parent、Bridge Token 和源。旧宿主缺少源信息时保留初始通配兼容，并在有效回执后绑定源。用 pagehide 清理挂起请求和轮询；保留沙箱内经典脚本兼容性。 | `ui/bridge.js`、`ui/app.js`、`tests/ui.optimization.test.mjs` |

### 6. 工程与发布

| 原建议 | 实施结果与行为 | 主要依据 |
| --- | --- | --- |
| §6.1 单一来源 | VERSION 驱动生成运行版本、package.json/lock 与 README 标记。内嵌 plugin.json 统一打包和运行元数据；defaults.json 集中推荐模型及别名，发现接口/Health 下发给 UI。增加一致性检查；打包排序改用标准排序。 | `internal/metadata/*`、`scripts/sync-metadata.py`、`cmd/package/main.go`、`tests/metadata.test.mjs` |
| §6.2 前端拆分 | 拆为 bridge、state、accounts、monitor、flow、app 六个经典脚本，HTML 显式顺序加载；不引入依赖沙箱模块跨源行为的 module 方案。保留共享宿主通信、草稿及接入流程约定。 | `ui/*.js`、`ui/index.html`、`tests/ui-fixture.mjs` |
| §6.3 测试与 CI | Unix socket 测试迁到 /tmp 短路径，仅明确报告不支持 socket 时跳过，权限/逻辑错误继续失败。新增配置、数字解析、SSE、分组模糊测试；热发布/冷快照/存储基准；100 目标×并发 8 合成场景及预算、重试、调度、停止、代际、存储恢复回归。CI 增加版本一致性、staticcheck、govulncheck、ruff、Go 缓存和基准产物；新增门禁的远端成功状态由发布阶段填写。 | `internal/*/*_test.go`、`tests/*`、`.github/workflows/ci.yml` |
| §6.4 工作区卫生 | 已在项目持久设置 `git config core.fileMode false`；实施前复查确认内容与基线一致，仅原优化文档未跟踪。不把文件系统产生的 100644→100755 漂移纳入功能差异。保留既有忽略规则，打包使用明确清单；最终 diff、暂存范围与工作树状态由发布门禁核验。 | `.gitignore`、`cmd/package/main.go`、Git 内容差异与发布检查 |

## 二、策略与兼容决定

### 判定、恢复与动作

- `family_match=false`。开启时只在预期参考类位于前两名且首位与其同家族时允许家族一致，结果明确标记；它不是模型身份认证。采样过程中关闭该选项，旧的家族匹配结果不能继续用于恢复。
- `recovery_spacing_seconds=0`。默认保留既有间隔；正值是显式策略选择。所有模式仍需连续两轮完整有效样本，未知/不确定轮次打断恢复确认。
- 管理面暂时性故障只暂停推进，不抹除已完成的恢复证据；确认不可见、无权限、非 active 或过期等条件不授权恢复。
- 关闭定时检测阻止后续自动调度；停止当前检测是独立动作，不解除账号既有暂停。新手动/停止命令通过已保存的新请求 ID 消费，旧命令在重启后不自动重放。
- 有完整评分的“不确定”和未收齐样本的“采样失败”分别计数。总体“需关注”可包含不确定，它不等价于网络失败数量。

### 状态文件、审计和预算

| 文件/投影 | 保留和恢复规则 |
| --- | --- |
| state.json | 紧凑的当前状态，关键提交仍经过临时文件同步、原子替换和目录同步；空记录、损坏 JSON 等不能静默重置。 |
| state.json.prev | 上一次已提交状态；仅提供可核对恢复的提示，不自动采用旧暂停归属或旧预算。 |
| audit.jsonl / audit.jsonl.1 | 独立审计追加与轮转。意图提交成功后才允许控制写。失败的部分追加在同一进程恢复重试时回到原已提交边界，再重写待提交事件。 |
| budget.json | 独立请求预留上限。每块最多 16 次，运行时显示实际已扣数；崩溃恢复采用保守上限，最多多计 15 次，正常关闭先持久化实际值再缩小上限。 |
| state.audit_events / activity_events | 快照分别下发审计与最近 300 条活动。活动不持久化。state.events 保留最近 300 条合并兼容投影，不作为审计保存容量。 |

升级时保持原数据目录、账号选择和自动处理开关；旧 events 中的审计迁移一次，历史记录不因分组移出而删除。旧 `suspect/paused_observation` 只在加载时迁移，已有暂停仍要重新读取和按规则验证。

回退前应正常停止插件，备份 **state.json、state.json.prev、budget.json、两个审计文件和桥接令牌**。旧版本不认识独立预算/审计文件，因此不能把“基础 state 仍为版本 1”理解为无条件可替换旧二进制。崩溃后的回退须核对 state.json 与 budget.json 的请求上限，不能通过恢复旧文件降低已经消费或保守预留的额度；旧版 UI 不展示独立审计时，文件仍需保留。

### 宿主与桥接

Health 继续返回完整账号列表，维护 Sub2API UI Bridge v1 兼容；收益来自预编码、修订号和增量 UI，并非已实施分页/条件获取。宿主按 ID 查询账号和原子条件改调度仍属于宿主侧能力扩展。

/healthz 成功只表示本机服务响应且令牌匹配；数据库只读权限、管理员账号授权、上游采样是不同层次。只读角色和非 root 服务模板已经交付，VPS 是否实际采用这些配置以部署记录为准。完整迁移步骤与回退入口见 [API-BRIDGE.md](API-BRIDGE.md)。

## 三、已完成的专项验证

专项验证使用本地合成账号、假管理端/上游及临时状态目录，不以真实账号调度写入或付费模型采样作为验收。

| 验证 | 已取得的结果 |
| --- | --- |
| Guard 全包竞态回归 | `go test -race ./internal/guard -count=1` 通过，28.372 秒；随后最终存储自恢复/空记录/软配置边界补充分别完成相应竞态回归。 |
| 运行时全包竞态回归 | 最终版本 `internal/runtime` 全包竞态验证 41 项通过，4.610 秒。 |
| 安全依赖复扫 | Go/网络/gRPC/protobuf 升级后，`govulncheck v1.9.0` 输出 `No vulnerabilities found`；具体版本见下表。 |
| Guard 静态检查 | `go vet ./internal/guard` 通过；staticcheck 指出的两个旧测试格式问题已修正，项目全量门禁另行记录。 |
| 调度与控制专项 | 慢账号不阻塞新到期账号；手动使用空闲名额并合并进行中轮次；软配置不重置启动期；关闭自动暂停/家族匹配在下一决策生效；停止不恢复既有暂停；分组更新不等待控制网络 I/O，旧代际不执行暂停。 |
| 存储专项 | 审计经过 5,000 条活动冲刷、轮转与重启仍保留；旧 events 只迁移一次；损坏/空记录不静默重置；prev 提示可用；预算崩溃上限保守恢复；临时目录不可写后能恢复运行。 |
| 100 账号×并发 8 | 使用完整采样/评分流程的合成上游，完成 100 轮、300 次请求；未泄漏预留额度；预算文件写入 ≤20 次，全量状态写入 ≤305 次；原审计事件保留。这里是测试上界断言，不是生产吞吐承诺。 |
| 模糊测试 | 数字解析 28,844 次、SSE 68,783 次、ParseConfig 55,691 次、TargetsForGroups 42,333 次执行通过。次数对应各自短时本地运行，不是穷尽证明。 |
| 打包/版本一致性 | 最终正式 0.4.0 包和完整本地套件已通过，51 项 Node 用例中正式归档检查执行且无跳过；远端 CI 和发布产物回执另见文末。 |

### 安全依赖升级与复扫

新增漏洞门禁实际发现了旧标准库和网络依赖中的可达漏洞，因此本轮同时更新工具链、直接/传递依赖和 vendor，再使用最终版本组合复扫：

| 组件 | 最终版本 |
| --- | --- |
| Go 工具链 | 1.26.10 |
| golang.org/x/net | v0.62.0 |
| golang.org/x/sys | v0.49.0 |
| golang.org/x/text | v0.43.0 |
| google.golang.org/grpc | v1.85.0 |
| google.golang.org/protobuf | v1.36.12 |

`govulncheck v1.9.0` 对上述最终组合扫描，输出 **`No vulnerabilities found`**。运行时全包竞态回归 **41 项通过，4.610 秒**。这些已取得的专项回执分别记录；最终全量本地门禁已通过，远端 CI 和发布部署联验在文末记录。

### 本地基准

环境：darwin/arm64、Apple M1 Max；100 条合成记录，基准命令：

```sh
go test ./internal/guard -run '^$' -bench 'Benchmark(Publish|Snapshot|Store)' -benchtime=100ms -count=1
```

| 项目 | 本次结果 | 分配 |
| --- | ---: | ---: |
| 进度发布 | 14.12 ns/op | 0 B，0 allocs |
| 未变更快照读取 | 101.0 ns/op | 16 B，1 alloc |
| 100 记录冷快照重建 | 812,235 ns/op（约 0.812 ms） | 472,864 B，1,327 allocs |
| 100 记录状态提交 | 9,664,705 ns/op（约 9.665 ms） | 65,545 B，533 allocs |

进度发布的工作已移动到按需快照读取；完整冷快照仍有复制和序列化成本。这些数字用于记录本机同一实现的成本分布。与原审查基准相比，结构、事件投影和提交内容已变化，不能把单个数值比值当作生产整体提速倍数。写盘次数减少、关键网络 I/O 不占全局锁、冷数据缓存命中率是本轮主要收益。

### 最终版本的 Health 规模基准

在 Apple M1 Max 上，对缓存账号列表的 `HealthCachedAccounts` 进行进程内合成基准，得到：

| 账号数量 | 时间（ns/op） | 分配（B/op） | allocs/op |
| --- | ---: | ---: | ---: |
| 100 | 46,059 | 86,023 | 68 |
| 1,000 | 208,944 | 520,721 | 68 |
| 10,000 | 1,626,740 | 4,848,353 | 72 |

这一组数据来自最终安全依赖升级后的版本组合。Health 使用缓存片段组包，同时保留完整 accounts 响应，因此账号增多仍会增加输出字节的分配和拷贝成本。上面的 Guard 基准是实现阶段的专项记录；两组数据用于各自的成本分析，不跨工具链或不同基准口径计算提速倍数。

## 四、最终发布与 VPS 回执（由发布阶段补充）

| 门禁 | 状态/证据 |
| --- | --- |
| 全量 make test、静态分析和依赖漏洞检查 | 最终 Go 1.26.10 / 安全依赖组合下 `make test` 通过：全包 `-race`、go vet、Node 51/51、Python 19/19，均无跳过。staticcheck v0.8.2、ruff 0.17.3（E4/E9/F）、元数据一致性检查通过；govulncheck v1.9.0 无已知漏洞。 |
| Linux amd64/arm64 构建、真实插件握手及包校验 | `make check-package`、`make package`、`make verify-package` 全部通过；本机真实插件握手、配置、Health 与 0.4.0 版本匹配。25 个有效负载文件哈希通过；无签名/测试签名包均验证成功，篡改、缺失、多余文件、错误签名及无效显式密钥均按预期拒绝。 |
| 提交 SHA、分支、v0.4.0 标签及推送 | 待补充最终回执 |
| 远端 CI 与 GitHub Release 资产校验和 | 待补充最终回执 |
| VPS 插件版本、进程健康、配置/灰度恢复 | 待补充最终回执 |
| VPS 桥接只读角色、非 root 进程和 /healthz | 待补充最终回执 |
| 回退备份位置与最终工作树状态 | 待补充最终回执 |
