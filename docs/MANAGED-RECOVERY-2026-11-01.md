# 已启用目标的统一调度管理（2026-11-01）

## 当前范围

用户于 2026-11-01 09:28:33 UTC 明确允许：对仍在指定分组、由插件管理的账号，即使用户或其他系统手动暂停，也在连续两轮有效通过后按恢复开关重新启用；删除或移出分组的不会恢复。此次风险确认只涉及代码，不包含生产上线。

本轮源码验证使用合成测试，不清除历史隔离、不强制恢复真实账号、不改变密钥/安全设置、不额外发起真实模型检测。新策略以用户本次最新要求为准，取代旧文档“外部暂停不接管、更新时间变化永久阻断恢复”的说明。

## 规则

- 只有已勾选并保存启用的目标纳入插件调度管理。未保存勾选不构成管理授权；取消勾选并保存后不再复测或恢复，已有暂停保持。
- 外部 schedulable=false 或 updated_at 变化不再形成永久恢复锁。旧 external_change / pause_uncertain 记录回到复测流程，不在升级时直接恢复。
- 连续两轮完整有效通过且自动恢复开启才可恢复。检测不可用不算通过，不新增指纹不符隔离；自动暂停与自动恢复开关保持独立。
- 已删除账号不重建；不可见账号不猜测、不给新身份。非 active 状态、已过期账号不通过调度恢复改写宿主限制。
- 暂停/恢复管理接口即使返回成功，也必须重新读取并确认实际调度状态；API Key 类型不能因精简响应缺少 type 而误走 OAuth 列表。
- 保留原预算、并发与重试设置。复测会消耗上游额度，不能把 UI 状态刷新当作真实采样验收。

## VPS 只读证据与边界

宿主运行版本为 Sub2API 0.2.19，commit dece26153621959ccd7635814f4b2d08304e681a，构建于 2026-10-28T10:09:03Z；运行二进制 SHA256 为 57b857ab51ecc22657423c1b8144abc96d666491aa10156fef7409b66d71c10f。镜像标签残留 0.2.6 不能代替运行版本。插件为 0.3.1。

一个 API Key 账号 在 2026-11-01 09:18:30 UTC+08 的 schedulable POST 返回 HTTP 200，但持久记录随后写入 pause_unconfirmed，原因“宿主范围内未找到该账号，保留当前调度状态”。之后 reconcile 记录暂停回执不确定；现场实际 schedulable=false，而暂停归属时间为空。HTTP 成功不等于插件完成后置确认。历史访问日志无响应正文，不为取证重发真实 POST。精确运行 commit 的公开源码区分两条路径：普通管理员 handler 返回完整 Account DTO，但受限管理密钥的 ScopedAdminKeyAccounts 拦截器对 schedulable POST 仅返回 id/status，不包含 type/schedulable；GET 详情仍包含 type。此契约与旧插件直接将写回执当完整账号、随后误走 OAuth 补全并报找不到账号的表现吻合。源码已确认这种不兼容机制，但未抓取现场响应正文，仍不冒充逐字在线回包证据。

另三账号的 updated_at 与额度刷新/usage 请求时间紧邻，且当时窗口未见对这些账号的调度或账号编辑调用。这是后台刷新关联证据，不足以证明最早一次外部变更保护的确切来源；保留此不确定性。

生产操作回执保留在私有诊断记录中；公开仓库不包含账号标识、凭据或内部操作编号。

## 精确宿主 commit 的公开协议核实

以下是公开源码契约证据，不是生产在线响应抓取：

- [DTO types.go](https://github.com/Wei-Shaw/sub2api/blob/dece26153621959ccd7635814f4b2d08304e681a/backend/internal/handler/dto/types.go)：Account 与 AccountListItem 的 `group_ids` 均带 `omitempty`，空组合法省略；不能把每个缺失字段都断言为损坏数据。
- [账号 handler](https://github.com/Wei-Shaw/sub2api/blob/dece26153621959ccd7635814f4b2d08304e681a/backend/internal/handler/admin/account_handler.go) 与 [repository](https://github.com/Wei-Shaw/sub2api/blob/dece26153621959ccd7635814f4b2d08304e681a/backend/internal/repository/account_repo.go)：普通管理员路径过滤参数为 `group=<正整数ID>`，不是 `group_id`；参数经 service 传至分组关联 WHERE 条件，应用后再统计和分页。受限管理密钥路径不同，见下条；不能只依赖服务端 group 查询。不要加 `status=active`，它还限制 `schedulable=true`，会漏掉待恢复账号。
- [response.go](https://github.com/Wei-Shaw/sub2api/blob/dece26153621959ccd7635814f4b2d08304e681a/backend/internal/pkg/response/response.go)：标准分页为 `data.items,total,page,page_size,pages`；pages 最少 1，空列表返回 `items:[]`。必须读完整分页再应用成员快照。
- [admin_account.go](https://github.com/Wei-Shaw/sub2api/blob/dece26153621959ccd7635814f4b2d08304e681a/backend/internal/service/admin_account.go)：普通调度修改后重新 GetByID 返回完整账户；受限密钥由独立 handler 改为最小写回执。
- [受限密钥 handler](https://github.com/Wei-Shaw/sub2api/blob/dece26153621959ccd7635814f4b2d08304e681a/backend/internal/handler/admin/custom_admin_key_accounts.go)：GET 列表仅使用 platform/type/status/search，忽略 group 参数；返回 items/total/page/page_size，不含 pages。page_size 范围 1–100，超出回退 20。DTO 的 group_ids 显式初始化 []，只列权限可见分组；必须客户端校验成员。POST schedulable 的成功 data 仅 id/status。两条认证路径不可混淆。

## 上线步骤（尚未执行，需单独授权）

1. 运行 Go、UI、桥接合成回归及构建/打包验证，记录新包版本与 SHA256；不能把旧包验签当作新代码已打包。
2. 核对已启用目标、自动暂停/恢复开关、原预算及并发，确保没有其他控制器争用调度。
3. 经授权备份原插件包与持久状态，按现行宿主更新流程替换插件；保留旧状态与审计事件，不清库、不重建账号、不提高权限。
4. 新策略上线后的自然复测仍须满足连续两轮有效通过；如需手动真实采样，应明确其额度消耗及授权。仅查 UI 或 GET 状态不能声称采样/恢复验收通过。
5. 核实每个目标的下一步、实际调度状态、恢复请求次数和回执。故障时停止进一步写操作，保留证据，按已授权回滚方案处理；不得为“修复界面”直接批量开调度。

分组成员沿用现有约 10 秒发现同步周期；只有完整成功快照确认后，才能认定成员已加入或移出。同步失败保留上次名单作为历史证据，但冻结新的采样与恢复，等待下一次完整同步。该流程不是宿主原子 CAS，无法保证成员变更瞬间零观察窗口；不要声称已消除读写竞态。
