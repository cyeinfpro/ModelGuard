# ModelGuard 0.4.1：移除监测后的列表归属

## 问题与修复

VPS 0.4.0 的实际配置中，13025、13026、13027 已经 enabled=false，当前监测目标为 13002、13024、13033。原 UI 将所有 hold/pending 记录无条件合入监测表，因此显示“3 个监测”却列出 6 行，并把已移除账号计入隔离数。

0.4.1 按已保存的有效目标决定监测列表归属；未保存的新选择仍明确显示“待保存”，不增加当前隔离与恢复确认计数。已移除目标（含暂停和待核对变更）进入折叠的“活动记录与诊断 → 未监测的历史记录”，保留上次原因及重新纳入监测的说明。历史记录展示不修改后台记录，不解除宿主已有暂停。

分组同步失败不是移除证据：继续展示上次确认成员及同步警告；仅在成功同步确认移出后归入历史。重新选择并保存后，账号重新出现在当前监测列表。

## 验证

- 51 项 UI 回归通过，包括生产六账号/三目标场景、保存移除与重新选择、待核对变更历史、未保存选择不计入隔离、分组移出与同步失败。
- `make test` 通过：Go 竞态回归与 vet、Node 53 项通过/1 项正式包缺失跳过、Python 19 项通过。
- `make check-package` 通过：Linux amd64/arm64 与本机运行验证、签名及无签名包清单和哈希、篡改与坏签名拒绝测试。
- 远端 CI 和 VPS 发布回执见下文。

不修改检测引擎、自动恢复策略、账号选择或 API 桥接源码。

## 发布与 VPS 回执

- 修复提交：[19b8e9c6bfd86fac7f087194867c7320a56e77ee](https://github.com/cyeinfpro/ModelGuard/commit/19b8e9c6bfd86fac7f087194867c7320a56e77ee)，main 与注解标签 v0.4.1 原子推送。
- [标签 CI 41976108757](https://github.com/cyeinfpro/ModelGuard/actions/runs/41976108757) 与 [main CI 41976108402](https://github.com/cyeinfpro/ModelGuard/actions/runs/41976108402) 均成功，包括静态分析、漏洞扫描与正式包验证。
- [正式 Release](https://github.com/cyeinfpro/ModelGuard/releases/tag/v0.4.1) 已发布；包大小 18,381,402 bytes，SHA-256 `cb0136e922d1b949e18fc8a23cf0e52790f850be1bd8fea18592fe117d1f9adb`。GitHub 资产 digest、SHA256SUMS 和 VPS 下载字节一致。
- 2026-11-05 13:48（北京时间）VPS 已从 0.4.0 更新到 0.4.1，enabled、runtime_healthy、api_bridge_ready 均为 true；runtime_error 与 accounts_error 均为空。
- 当前监测目标为 13002、13024、13033；13025、13026、13027 仍 enabled=false，原 hold 与宿主 schedulable=false 保留。配置摘要、100% 灰度、宿主容器、核心二进制与桥接容器均未改变；上传后启动前 state.json 字节一致。
- 安装后的 ui/monitor.js 与发布包逐字节一致，SHA-256 `27de561a69c2653abc69187b98081fef0ebf9703bc28919b6f852d5b0c70811e`。生产数据对应的 DOM 行数与归属由回归测试覆盖；本轮未操作用户浏览器进行像素级验收。
- VPS 回退备份：`/root/sub2api-deploy/backups/modelguard-041-20261105T054850Z/`，包含旧插件包、插件表、停用后的状态与 verification.json。发布标签保持不变，回执另以文档提交归档。
