# ModelGuard 深度优化建议（2026-11-05）

日期：2026-11-05（北京时间）
基线：`eba5599`（Release v0.3.2），`VERSION = 0.3.2`
性质：**只读审查 + 建议**。本文不修改任何源码、配置或生产状态；所有结论都标明了依据（源码位置、实测数据或推断）。

---

## 0. 结论速览

ModelGuard 的“安全语义”已经做得很细：暂停归属、写后复读、不跟随跳转、原子落盘、手动请求不重放、分组同步冻结等，都有明确设计和测试。**下一阶段的主要收益不在继续加保护，而在三件事：**

1. **别浪费上游额度**：配置类错误和不可重试的 HTTP 错误目前也会扣预算并重试 2 次。
2. **把热路径从“全量序列化 + 全量 fsync”里解放出来**：每次进度刷新都整包序列化状态，每条采样日志都整包写盘；审计事件还会被采样日志挤掉。
3. **调度从“批次屏障”改成“持续工作池”**：一个慢账号会拖住整批、手动检测和配置保存。

| # | 优先级 | 问题 | 一句话建议 | 工作量 |
| --- | --- | --- | --- | --- |
| 1 | P0 | 配置错误也先扣预算，且照样尝试 3 次（§2.1） | 先判定、后预算；配置错误立即终止本轮 | 小 |
| 2 | P0 | 401/403/404/400 等不可重试错误照样重试，间隔仅 1 s / 2 s（§2.2） | 错误分级 + 指数退避 + 遵守 `Retry-After` | 小 |
| 3 | P0 | 审计事件（暂停/恢复）与采样日志共用 300 条环形缓冲，一轮巡检即可被挤出（§2.4） | 审计事件与采样日志分离存储 | 小–中 |
| 4 | P1 | “评分不确定”与“采样失败”共用 `unavailable` 状态（§2.3） | 新增 `inconclusive`，分开计数与展示 | 小 |
| 5 | P1 | `publishLocked` 在全局锁内整包 JSON 序列化，28 处调用，进度回调每 500 ms 触发（§3.1） | 脏标记 + 读时构建快照 | 中 |
| 6 | P1 | `Store.Save` 每次整包 `MarshalIndent` + 双 fsync，实测 100 账号 ≈ 11.7 ms/次（§3.2） | 关键状态同步写，日志类合并写 / 追加写 | 中 |
| 7 | P1 | Health 先反序列化快照再整包重序列化，并附带全部账号列表，UI 每 3 s 拉一次（§3.3） | `json.RawMessage` 拼装 + 修订号 | 小 |
| 8 | P1 | 账号发现无论模式都每 10 s 全量分页，401/429 也不退避（§3.4） | 自适应周期 + 错误退避 | 小 |
| 9 | P1 | 批次屏障：慢账号阻塞整批、手动检测与保存配置（§4.1、§4.2） | 持续调度工作池；软配置热更新 | 大 |
| 10 | P1 | 生产 OAuth 转发与探测共用传输池，转发路径硬编码 180 s 首包超时（§3.6） | 转发与探测分池；转发超时与宿主对齐 | 小 |
| 11 | P1 | API 桥接以 root + `docker exec psql` 读取整个 `credentials` JSON（§5.1） | 只读库角色、只选必要字段、非 root 运行 | 中 |
| 12 | P2 | 版本号、模型默认值、端口 8080 多处硬编码且已出现漂移（§5.2、§6.1） | 单一来源 + 一致性测试 | 小 |

建议排期见 §8。

---

## 1. 审查范围与方法

| 项目 | 内容 |
| --- | --- |
| 源码 | `internal/guard`、`internal/runtime`、`internal/fingerprint`、`internal/pluginv1/runtime.go`、`cmd/*`、`ui/*`、`scripts/*.py`、CI 与 Makefile，逐文件通读 |
| 文档 | README、`docs/REFACTOR_2026-10-30.md`、`docs/MANAGED-RECOVERY-2026-11-01.md`、`docs/API-BRIDGE.md` |
| 工作区 | 工作区内容与 `eba5599` 一致；`git diff` 显示 955 个文件仅有权限位变化（100644 → 100755），无内容改动，见 §6.4 |
| 实测 | 在项目副本上运行临时基准（不写入项目），见 §3.1、§3.2；完整测试结果见 §7 |
| 未做 | 未发起任何真实模型请求，未连接生产 Sub2API，未修改账号或状态文件 |

整体结构（便于对照后文）：

```
Sub2API 宿主 ──gRPC──► Plugin(runtime)
   │                     ├─ Forward()          生产 OAuth 流量透传（共享 ClientPool）
   │                     ├─ Health()/TestConfig/ApplyConfig   UI Bridge 后端
   │                     ├─ discoveryLoop       每 10 s ListAccounts 全量分页
   │                     └─ Engine(guard)
   │                          ├─ Run: 5 s 定时 Tick → runTargets(批次, 并发 1–8)
   │                          ├─ tickTargets → Admin.Account → Evaluate(3 样本 × ≤3 次)
   │                          ├─ pause/recover（controlMu 串行）
   │                          └─ Store: state.json（整包写 + fsync）
   └─ HostService(ResolveOutboundIdentity / ListAccounts)
API Key 账号 ──► Unix Socket ──► api-credential-bridge.py ──► docker exec psql
```

---

## 2. 正确性与策略

### 2.1 配置类错误也扣预算，并且照样尝试 3 次〔P0〕

**现象**

- `internal/guard/probe.go:304`：`Evaluate` 在调用 `probe.Sample` **之前** 执行 `reserve(...)`，`reserve` 内部调用 `Store.ReserveRequest`，请求数立刻 +1 并落盘。
- `Prober.SampleWithProgress` 中多种失败发生在发出 HTTP 请求之前：宿主身份解析失败、`base_url` 无效、无出站凭据（返回 `ProbeConfigurationError`），以及代理地址无效（返回普通错误，会被当成暂时性失败）。
- `probe.go:331` 只在 **第 3 次尝试之后** 才检查 `ProbeConfigurationError`；前两次照常等待、照常扣预算。

**影响**：一个持续处于此类错误的账号，每轮会白扣 3 次预算（第 1 份样本就失败，再重试 2 次），且每 `retry_seconds`（默认 2 分钟）重来一次：一天约 3 × 720 = 2160 次，**单账号即可耗尽默认 2000 次的日预算**，连带其他账号全部 `blocked`。这是按源码链路推算的结果，未在生产上复现。

**建议**

1. 把“出站身份/端点/凭据解析”从 `Sample` 中拆成 `Prepare(ctx, account, target) (preparedRequest, error)`，在 **预算预留前** 执行。
2. `Evaluate` 遇到 `ProbeConfigurationError` 立即返回，不进入重试循环。
3. 预算只在 **请求真正发出**（`client.Do` 被调用）后计数；连接都没建立的失败，按需求决定是否退还。

**验证**：单元测试让 `Resolve` 返回错误，断言 `State.Requests == 0`、`Sample` 只调用一次。

### 2.2 不可重试的上游错误也重试，退避过短〔P0〕

**现象**

- `probe.go:164`：非 200 一律返回 `上游 HTTP %d`，丢弃响应体；`Evaluate` 对所有错误一视同仁地重试。
- `probe.go:295`：重试间隔为 `attempt-1` 秒，即 1 s、2 s。
- 没有读取 `Retry-After`。

**影响**：401（Key 失效）、403、404 / 400（模型名不存在、映射错误）这类 **重试必然失败** 的错误，每轮浪费 2 次额外预算；429 在 1–2 s 内连续重试，容易延长限流窗口。页面只能看到“上游 HTTP 400”，用户不知道是模型名错了还是参数错了。

**建议**

| 类别 | 例子 | 处理 |
| --- | --- | --- |
| 永久性 | 400、401、403、404、422 | 不重试；作为配置错误返回，并附带脱敏后的 `error.code`/`error.type`（只取白名单字段，限 200 字节） |
| 限流 | 429 | 读取 `Retry-After`（上限如 60 s），超过本轮剩余时间则直接结束本轮 |
| 暂时性 | 5xx、连接重置、流中断、超时 | 指数退避 + 抖动，例如 5 s、15 s |

**验证**：表驱动测试覆盖各状态码的重试次数与预算消耗。

### 2.3 “评分不确定”与“采样失败”混成同一状态〔P1〕

**现象**

- `fingerprint/detector.go:619–624`：只有 top-1 等于预期才算 `match`；top-1 置信度 ≥ 0.90 且预期概率 < 0.03 才算 `mismatch`；其余都是 `indeterminate`（“候选模型接近”）。
- `guard/state.go:184`：`Transition` 把 `indeterminate` 与网络失败、样本不足一样记为 `status = "unavailable"`、`Unavailable++`。
- UI 把 `unavailable` 显示为“检测失败”。

**影响**

- 三份样本都完整采集、只是相近家族（例如同代不同档位）排序接近时，用户看到“检测失败”，会去排查网络和凭据，方向完全错误。
- 隔离中的账号若长期落在 `indeterminate`，既不累计恢复轮次，也不报不符，容易长期卡住，且原因不可见。
- 统计口径混淆，无法回答“采样成功率是多少”“判定不确定率是多少”。

**建议**

1. `Transition` 新增 `inconclusive` 状态（不计入 `Unavailable`，单独计数 `Inconclusive`），保留 `Analysis.Candidates` 供 UI 展示前 3 名及概率。
2. UI 文案：“已完成采样，但参考库无法可靠区分（前三：A 41% / B 37% / …）”。
3. 产品层面可评估 **家族级匹配** 选项：预期为 X 时，若 top-1 与 X 同 `Family` 且 X 概率位列前 2，可判为“家族一致”。这会改变判定语义，需要单独决策，默认不开启。

### 2.4 审计事件会被采样日志挤出〔P0〕

**现象**

- `state.go:133`：`Events` 为全局 300 条环形缓冲。
- 每份样本写 `sample_started` + `sample_finished` 2 条，重试再加 `sample_retry`；每账号每轮至少 7 条（6 条采样 + 1 条 `check`）。
- 暂停/恢复相关的 `pause_intent`、`paused`、`pause_unconfirmed`、`recovery_intent`、`recovered`、`managed_pause` 与它们共用同一缓冲。

**影响**：**43 个账号跑完一轮即写满 300 条**；100 个账号一轮约 700 条。于是“谁在什么时候暂停/恢复了哪个账号”的证据在一轮之内就被采样日志冲掉。`MANAGED-RECOVERY` 文档里依赖的正是这类事件做取证。

**建议**

1. 拆为两类存储：
   - `audit`：暂停/恢复/归属变化/归档/配置应用，**不与采样日志共享容量**，保留 ≥ 2000 条或 30 天。
   - `activity`：采样开始/结束/重试，保留最近 N 条，可只存内存或单独文件。
2. 审计事件改为追加写 `audit.jsonl`（`O_APPEND` + fsync），按大小轮转；`state.json` 只存当前状态（另见 §3.2）。
3. UI 的“活动记录”默认展示 `audit`，“显示采样”再合并 `activity`。

### 2.5 恢复确认的两轮间隔过短（策略建议，需决策）〔P2〕

`state.go:163`：隔离账号第 1 轮通过后，下一次复测在 `retry_seconds`（默认 120 s）后。两轮“连续有效通过”实际可能只相隔两三分钟。对于“间歇性换模型”的渠道，两次时间上紧邻的采样独立性有限。

**建议**：新增 `recovery_spacing_seconds`（例如默认 1800 s），两轮恢复确认之间至少间隔该值；或要求两轮落在不同的上游连接/时间窗。这是策略改变，需要与 2026-11-01 的授权口径一起确认。

### 2.6 状态机遗留与重复分类〔P2〕

| 位置 | 问题 |
| --- | --- |
| `state.go:179` | `suspect` 被赋值后立即被 `pause_recommended`/`quarantined` 覆盖，永远不会持久化 |
| `pipeline.go` / UI | `paused_observation`、`external_change` 在 Go 中已无任何赋值点，只剩兼容读取 |
| `manual.go:107` vs `pipeline.go:24` | 同一套“需要关注”的状态列表写了两遍，手动统计与批次统计可能漂移 |
| `engine_targets.go` | 新增了 `archived`、`pending` 状态，但 UI `names` 映射里没有，界面会直接显示英文原值 |
| `tickTargets` | 管理端一次读取失败就把 `RecoveryPasses` 清零；控制面抖动会让隔离账号的恢复进度反复归零 |

**建议**：把状态集中为 `type Status string` 常量 + 一张“状态 → 类别（ok / attention / held / archived）”表，Go 与 UI 共用（Health 中下发 `status_catalog`）；`suspect` 等遗留值只在加载旧状态时迁移一次。管理端读取失败是否清零恢复轮次，建议改为“不清零但不推进”，并单独决策。

### 2.7 校准参数缺失时静默降级〔P2〕

`detector.go:596`：`calibrate` 返回 `nil` 时每一轮都得到 `indeterminate`（“评分器校准参数不可用”），但 `fingerprint.Load()` 并不检查校准。参考库文件若被替换或损坏到这一层，插件会一直“检测失败”而不报启动错误。**建议**：在 `Decode` 中校验 `Calibration` 绑定，失败即返回错误，由 `runtime.New` 让插件启动失败、Health 明确报告。

---

## 3. 性能与资源

### 3.1 进度发布：全局锁内整包序列化〔P1〕

**现象**

- `engine.go:109`：`publishLocked` 每次都 `json.Marshal` 整个 `Store.State`（全部记录 + 300 条事件）、批次、预算、64 个模型元数据，在 `stateMu` 内执行。
- 共有 28 处直接调用；`probe.go:210` 的进度回调每 500 ms 触发一次，每个并发采样各一路。

**实测**（项目副本 + 临时基准，darwin/arm64，100 条记录 + 300 条事件）：

| 指标 | 数值 |
| --- | --- |
| 快照大小 | 162,580 B |
| `publishLocked` 单次耗时 | ≈ 0.44 ms |
| 单次分配 | ≈ 210 KB / 762 次 |

8 路并发时进度回调约 16 次/s，即约 3.4 MB/s 的分配和约 7 ms/s 的持锁时间；这些时间里其他 worker 的记录更新、预算预留都在等锁。单看 CPU 不算大，但它和 §3.2 的 fsync 叠加在同一把锁上，且随账号数线性增长。

**建议**

1. `publishLocked` 只设置 `dirty = true` 并更新轻量字段（`active_samples`、`batch`）；**快照在 `Snapshot()` 被读取时按需构建**，并缓存到下一次变脏。Health 每 3 s 读一次，序列化频率即从“每次事件”降到“每次读取”。
2. 将“热数据”（活动采样进度）与“冷数据”（记录、事件）分开序列化，热数据单独一个小结构。
3. `Detector.Models()` 每次复制 64 项再序列化，可在加载时预先序列化为 `json.RawMessage`。

### 3.2 持久化：整包 `MarshalIndent` + 双 fsync〔P1〕

**现象**

- `state.go:101`：`Save` 每次 `MarshalIndent` 整个状态，写临时文件、`fsync`、`rename`、再 `fsync` 目录。
- 共有 36 个 `e.save()` 调用点。每份样本至少 3 次 `Save`：`ReserveRequest`、`sample_started`、`sample_finished`；一轮账号检测 ≥ 10 次。

**实测**（同上环境）：100 条记录时 `state.json` 为 235,385 B，`Store.Save` 单次 ≈ 11.7 ms。注意 macOS 上 Go 的 `Sync` 使用 `F_FULLFSYNC`，Linux 生产环境通常更快，但写放大比例不变：**每份样本约 0.7 MB 磁盘写入**，100 账号一轮约 200 MB。

**建议**

| 数据 | 落盘策略 |
| --- | --- |
| 暂停/恢复意图与结果、记录状态转换、手动请求入队 | 保持同步原子写（这是安全语义的根基，不要动） |
| 预算计数 | 合并写：每 N 次或每 1 s 写一次；进程重启时按“已预留未确认”的保守值恢复 |
| 采样开始/结束日志 | 只进内存 `activity`，或追加写 `activity.jsonl`，不触发 `state.json` 重写 |
| 格式 | `json.Marshal` 代替 `MarshalIndent`（体积约 −25%，并省去缩进开销） |

另外，`state.json` 损坏时目前是“保留原文件并停止监测”，安全但无自愈。**建议** 每次成功提交后保留 `state.json.prev`，加载失败时给出“可从上一版恢复”的明确提示（不自动采用）。

### 3.3 Health：反序列化后重序列化，且附带全部账号〔P1〕

**现象**

- `runtime/plugin.go:123`：`Health` 把引擎快照 `json.Unmarshal` 成 `map[string]any`，再合并约 30 个字段后整体 `json.Marshal`。
- `plugin.go:173`：附带完整 `accounts` 列表，最多 10,000 条。
- `TestConfig` 的 `testResponse` 再把 Health 结果反序列化、重序列化一遍。
- UI `app.js:442` 每 3 s 拉取一次；`app.js:133` 再对整份账号列表 `JSON.stringify` 求签名。

**建议**

1. 用结构体 + `json.RawMessage` 拼装：引擎快照原样嵌入，不做 Unmarshal。
2. 账号列表在每次发现成功后预先序列化，并计算 `accounts_revision`（内容哈希）；Health 下发修订号，UI 用修订号比较，不再 stringify 全量。
3. 宿主 `plugin.status` 无参数，无法做条件请求；但可以把 Health 拆为“摘要（每 3 s）”与“账号列表（修订号变化时才需要）”两层：例如只有当 UI 处于“管理账号”面板展开时，才通过 `config.test` 的诊断回执取完整账号列表。此项需评估宿主契约，列为可选。

### 3.4 账号发现：固定 10 s 全量分页，无退避〔P1〕

**现象**：`plugin.go:274`，只要填了管理密钥，无论手选模式还是分组模式，都每 10 s 拉一次完整分页（每页 100 条，最多 100 页）。失败（包括 401、403、429）后同样 10 s 后重试。

**影响**：10,000 个账号的站点，每 10 s 约 100 次管理接口请求；密钥失效时也持续打 401；429 时继续加压。

**建议**

| 场景 | 周期 |
| --- | --- |
| 分组模式且同步正常 | 10–30 s（保留现有语义） |
| 手选模式 | 60–300 s；UI 打开“管理账号”或点击“重新加载”时立即刷新 |
| 401 / 403 | 停止自动轮询，直到配置变更或用户手动重试 |
| 429 / 5xx / 网络错误 | 指数退避（10 s → 20 s → … 上限 5 min），遵守 `Retry-After` |

`connectionStatus` 的 45 s “过期”阈值需随周期调整为“周期 × 3”。

### 3.5 OAuth 元数据补全：每次读账号都拉宿主全量列表〔P2〕

`runtime/account_metadata.go:24`：`enrichAccount` 调用宿主 `ListAccounts(platform, type)`，返回该平台/类型的 **全部** 账号元数据后线性查找一个 ID。`admin.go:95` 在管理接口未返回 `updated_at` 时触发它；`Pause`/`Resume` 的写后复读也会再触发一次。`ListAccountsRequest` 目前没有 ID 过滤字段（`plugin.proto:206`）。

**建议**：短 TTL（如 5 s）缓存宿主列表，同一轮内的多次补全共用一次结果；写后复读仍需强制刷新，以免读到旧值。长期可向宿主提议在 `ListAccountsRequest` 增加 `account_ids` 过滤。

### 3.6 生产转发路径与探测共用传输池〔P1，涉及业务流量〕

**现象**

- 插件参与 `openai.oauth.outbound_transport.v1` 绑定；绑定生效后，宿主的 OAuth 出站业务请求经由 `runtime/forward.go` 的 `Forward` 转发，插件处在业务流量的关键路径上。
- `Forward` 使用 `p.pool.Client(proxy)`，而该池（`probe.go` 中 `ClientPool`）为探测设计：`ResponseHeaderTimeout = 180 s`（`probe.go:65`）、`MaxIdleConnsPerHost = 4`（`probe.go:63`）、超过 64 个代理时随机淘汰一个传输。
- 探测本身反而不用池里的连接：每份样本都 `Clone()` 并 `DisableKeepAlives`（`probe.go:154`）。

**影响**

- 非流式、长推理请求若 180 s 内未返回响应头，业务请求会被插件判为 `upstream_transport_error`。这是插件替宿主做出的超时决定，宿主侧未必知情。
- 同一代理下并发 > 4 时空闲连接会被频繁关闭重建（HTTP/2 下影响较小，HTTP/1.1 代理下明显）。

**建议**

1. 转发与探测 **分池**：转发池不设 `ResponseHeaderTimeout`（由宿主传入的 `stream.Context()` 控制取消），`MaxIdleConnsPerHost` 提高到 32 或可配置。
2. 探测既然每次新建连接，就不需要池化，直接用一个模板 `Transport` 克隆即可，`ClientPool` 只服务转发。
3. 代理淘汰改为 LRU，避免淘汰正在热用的代理。
4. 为转发补充最小可观测性：在 Health 中加入转发计数、错误码分布、首包延迟分位数（只计数，不记录内容）。

### 3.7 UI 渲染〔P2〕

- `render` 每 3 s 整表 `replaceChildren` 重建监测记录与事件列表；`nextAction` 和事件循环里用 `accounts.find`，复杂度为“记录数 × 账号数”。
- 建议：建 `Map` 索引；记录行按 `account_id` 复用 DOM，只更新变化的单元格；用 Health 下发的修订号决定是否重绘账号表。

---

## 4. 调度与可用性

### 4.1 批次屏障〔P1〕

`parallel.go` 的 `runTargets` 以“批”为单位：取出当时到期的账号，开 N 个 worker 跑完，**整批结束后** 才会再看下一批。`Run` 在同一个 goroutine 里处理手动队列与定时器。

**影响**

- 单账号最坏耗时 ≈ 3 份样本 × 3 次 × `timeout_seconds`(180 s) + 重试等待 ≈ 27 分钟。批内其他 worker 空闲后只能等它。
- 这期间到期的其他账号、隔离复测、恢复确认全部延后。
- 手动检测排在当前批次之后（`manual.go` 中文案“当前巡检结束后开始”）。

**建议**：改为 **持续调度器**——一个按 `NextAt` 排序的最小堆 + 固定 N 个 worker，worker 空闲即取下一个已到期账号；手动检测只是把选中账号的 `NextAt` 置为现在并标记来源。`BatchState` 改为滚动统计（“近 1 小时完成 / 失败 / 进行中”）。现有的 `inFlight`、`generation`、预算预留机制可以原样复用。

### 4.2 任意配置修改都要等空闲并重启引擎〔P1〕

`plugin.go:321`：只要引擎在跑批次，`ApplyConfig` 就返回“检测执行中…请等待本轮完成后保存”。即使只切换“自动恢复”开关，也会 cancel 整个引擎、重开状态目录、重新进入 60 s 启动保护期（`engine.go:79`）。

**建议**：把配置分为两类：

| 类别 | 字段 | 应用方式 |
| --- | --- | --- |
| 软配置 | `enabled`、`auto_pause`、`auto_recover`、各类间隔、`concurrency`、`daily_requests`、`timeout_seconds`、目标模型 | 原子替换 `Engine.Config`（`atomic.Pointer`），下一次决策即生效，不中断进行中的采样 |
| 硬配置 | `admin_url`、`admin_key`、`data_dir` | 保留现有“挂起 → 重启”流程 |

目标增删可复用 `UpdateManagedTargets` 的 generation 机制：被移除的账号让进行中的采样自然作废。另外建议提供 **“停止当前检测”** 操作，取消进行中的批次（不改变已有暂停）。

### 4.3 全局 `controlMu` 串行化所有暂停/恢复的网络 I/O〔P2〕

`engine.go:434`、`engine.go:502`：`pause`/`recover` 持有全局 `controlMu` 期间做 2–3 次管理接口调用（每次超时 15 s）。`UpdateManagedTargets` 与 `SetGroupSyncBlocked` 也要拿这把锁，所以一次慢的暂停会让分组同步一起等待。

**建议**：`controlMu` 只保护“生成号校验 + 意图落盘”这一小段；网络 I/O 改为按账号加锁（`inFlight` 已经保证同账号串行）。分组同步在取锁后发现目标代际变化，进行中的暂停在写前复核时自然放弃。

---

## 5. 安全与权限

### 5.1 API 凭据桥接的最小权限〔P1〕

**现象**（`scripts/api-credential-bridge.py`）

- 第 127 行：每次请求 `docker exec <container> psql -U sub2api -d sub2api`。服务需要 Docker 访问权限，实际等同 root；库用户名、库名硬编码。
- 第 119 行：返回整个 `credentials` JSON，而插件只用 `api_key`、`base_url`、`model_mapping`。
- 每次读取都要起一个 `docker exec` 子进程。

**建议**

1. 在 PostgreSQL 中建只读角色，只授予 `accounts`、`proxies` 两表所需列的 `SELECT`。
2. 桥接直接以该角色连接数据库（Unix socket 或回环 TCP），不再需要 Docker 权限，可用专用非 root 用户运行；systemd 单元增加 `User=`、`CapabilityBoundingSet=`、`PrivateTmp=`、`RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`。
3. SQL 只投影必要字段：`credentials->>'api_key'`、`credentials->>'base_url'`、`credentials->'model_mapping'`。
4. `-U` / `-d` 改为环境变量。
5. 增加无秘密的 `GET /healthz`（只验证令牌、返回 `{"ok":true}`），让 Health 能区分“socket 文件存在”和“服务在线”，不必等真实检测才发现桥接挂了。

### 5.2 管理地址端口限制前后不一致〔P2〕

`guard/api_bridge.go:23` 与 `runtime/status.go:184` 只接受 `http://127.0.0.1:8080` / `http://localhost:8080`，而桥接脚本接受任意回环地址和端口（含 `::1`）。管理端口不是 8080 的部署无法使用 API 桥接，且报错为“要求本机管理地址”，容易误导。**建议**：插件端改为“回环主机 + 任意端口”，与桥接脚本保持同一规则，并抽成一个共享函数。

### 5.3 UI `postMessage` 目标源为 `*`〔P3〕

`ui/app.js:23` 等处以 `targetOrigin = "*"` 向父窗口发送消息，其中 `config.save` 的载荷包含管理员密钥。接收端已校验 `e.source === parent` 和 Bridge Token，且页面运行在沙箱 iframe 中，实际风险低。**建议**：若宿主可在 URL hash 中下发父页面 origin（或从 `location.ancestorOrigins[0]` 读取），则固定目标源；否则维持现状并在文档中注明原因。另外 `unload` 事件已被浏览器逐步弃用，建议改用 `pagehide`。

---

## 6. 工程与发布

### 6.1 版本与默认值的单一来源〔P2〕

| 漂移点 | 现状 |
| --- | --- |
| 版本 | `VERSION`、`package.json`、`runtime.Version`、`manifest` 均为 0.3.2；**README 第 5 行仍写 0.3.1** |
| 插件元数据 | `id`、`name`、`requires`、`capabilities` 在 `cmd/package/main.go` 中硬编码一份，`runtime/plugin.go` 又有 `ID`、`Capability` 常量 |
| 推荐模型 | `discovery.go:68–71`（`gpt-6.1-terra`、`gpt-6.1-sol` → `gpt-6-astra`）与 `ui/app.js:68,77,418` 各写一份 |
| 模型别名 | `detector.go:532` 硬编码 `gpt-6.1-sol` 等别名 |

**建议**

- 新增 `plugin.json`（或 `manifest.template.json`）作为元数据唯一来源，打包与运行时都从它读取（运行时可 `go:embed`）。
- 推荐模型、别名表放进一个随参考库一起版本化的 `defaults.json`；UI 只用服务端下发的 `suggested_*`，去掉 JS 中的回退字面量。
- 加一个测试：README 中的“当前工作版本”必须等于 `VERSION`。

### 6.2 前端可维护性〔P2〕

`ui/app.js` 共 463 行、46 KB，大量单行函数；`tests/ui.test.mjs` 36 KB 也难以定位。**建议**：按“桥接通信 / 状态模型 / 账号表 / 记录表 / 接入流程”拆成多个经典脚本（按顺序引入），或引入零配置打包（如 esbuild）输出单文件。沙箱 iframe 中使用 `type="module"` 可能受跨源限制，需要先在宿主实测再决定。

### 6.3 测试与 CI〔P2〕

现有覆盖已经不错（Go 共 94 个 `Test*` 函数、Node 39 项、Python 桥接 14 项）。

**先修一个测试脆弱点**：`internal/runtime/flow_test.go`（`os.MkdirTemp(".", ".mg-")`）和 `tests/api_bridge_test.py:28`（`TemporaryDirectory(dir=项目根)`）都把 Unix socket 建在项目目录里。项目若放在不支持 socket 的文件系统上（网络盘、部分同步盘），`make test` 会报 7 个与代码无关的失败（§7 实测）。建议改为在 `/tmp` 下建短路径临时目录（同时满足 macOS 104 字节路径限制），绑定返回 `EOPNOTSUPP` 时 `t.Skip` 并说明原因，而不是报失败。

再建议补充：

| 类型 | 目标 |
| --- | --- |
| 模糊测试 | `parseNumbers`、`readResponsesProgress`、`ParseConfig`、`TargetsForGroups` |
| 基准测试 | 把本文 §3.1、§3.2 的临时基准正式纳入，并在 CI 中记录趋势 |
| 规模测试 | 100 目标 × 并发 8 的合成全流程，断言持锁时间、写盘次数、事件保留 |
| 预算测试 | §2.1、§2.2 的预算不被浪费 |
| CI | `govulncheck`、`staticcheck`、Python `ruff`；为 `go test -race` 开启构建缓存 |

另：`cmd/package/main.go` 的手写插入排序可直接用 `sort.Strings`。

### 6.4 工作区卫生〔P3〕

- 工作区 955 个文件的权限位从 100644 变为 100755（内容无变化），疑为跨文件系统同步导致。`git status` 会一直显示大量修改，容易掩盖真实改动。建议执行 `git config core.fileMode false`，或批量恢复权限位。
- `tests/__pycache__`、多处 `.DS_Store` 已被 `.gitignore` 忽略。`cmd/package` 只收录 `ui/`、`licenses/` 与固定文件清单，目前不受影响；若今后用目录级方式生成源码归档，需显式排除这类文件。

---

## 7. 验证记录

以下为本次在项目工作区实际执行的结果：

| 检查 | 在项目目录内运行 | 在 `/tmp` 副本中运行 |
| --- | --- | --- |
| `go vet ./...` | 无输出（通过） | — |
| `go test -race ./internal/fingerprint` | 通过 | — |
| `go test -race ./internal/guard` | 通过（30.2 s） | — |
| `go test -race ./internal/runtime` | **1 项失败**：`TestBridgeReadinessRequiresSocketAndPrivateTokenButDoesNotClaimConnectivity`，`bind: operation not supported` | 通过 |
| `npm test` | 39 项全部通过 | — |
| Python 桥接 `unittest` | **6 项错误**，均为 `OSError: [Errno 45] Operation not supported` | 14 项全部通过 |

所有失败只有一个原因：当前项目目录所在的文件系统不支持创建 Unix socket，测试却把 socket 建在项目目录下（Go 测试用 `os.MkdirTemp(".", ".mg-")`，注释说明是为了避开 macOS 的 socket 路径长度限制）。代码本身无回归，但测试对运行目录有隐含要求，见 §6.3 的修复建议。

基准测试在项目副本中运行（临时目录，测试文件未写入项目），环境为 darwin/arm64（10 核）、Go 1.26.1：

| 基准 | 结果 |
| --- | --- |
| 10 条记录 + 300 条事件 | `state.json` 74,667 B；快照 62,972 B |
| 100 条记录 + 300 条事件 | `state.json` 235,385 B；快照 162,580 B |
| `publishLocked`（100 条记录） | 442,176 ns/op，210,424 B/op，762 allocs/op |
| `Store.Save`（100 条记录） | 11,734,487 ns/op，534,707 B/op，723 allocs/op |

这些数字反映的是开发机，Linux 生产环境的 fsync 耗时通常更低，但序列化开销与写放大的比例关系不变。

---

## 8. 建议排期

**第一阶段：止损（约 1–2 天，不改变判定语义）**

1. §2.1 先判定后预算；配置错误不重试。
2. §2.2 错误分级、退避、`Retry-After`；上游错误码白名单透出。
3. §2.4 审计事件与采样日志分离。
4. §3.4 发现周期自适应与退避。
5. §3.6 转发与探测分池，去掉转发的 180 s 首包超时。
6. §6.1 README 版本号修正，加一致性测试。
7. §6.3 socket 测试改用 `/tmp` 短路径，不再依赖项目目录的文件系统。

**第二阶段：结构（约 1 周）**

1. §3.1 / §3.2 快照按需构建；落盘分级。
2. §3.3 Health 拼装改为 `RawMessage` + 修订号；UI 按修订号重绘。
3. §2.3 新增 `inconclusive` 状态与 UI 展示。
4. §4.2 软配置热更新 + “停止当前检测”。
5. §5.1 桥接只读角色、字段投影、非 root、`/healthz`。

**第三阶段：架构（视需要）**

1. §4.1 持续调度器替换批次屏障。
2. §4.3 `controlMu` 收窄。
3. §2.5 恢复间隔、§2.3 家族级匹配（需产品决策）。
4. §6.2 前端拆分。

每一项上线前都应沿用现有原则：合成测试先行；不以 UI 刷新代替真实采样验收；涉及暂停/恢复语义的改动单独授权、单独发布。
