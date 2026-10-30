# ModelGuard · 模型哨兵

Sub2API 原生模型质量监测插件。自动巡检指定账号的模型指纹，首轮完整采样确认不符后暂停调度，连续两轮有效通过后自动恢复。

## 功能

- 自动读取管理密钥授权范围内的账号，勾选后加入监测。
- 默认 3 个账号并行，可调 1–8；手动检测与定时检测共用并发上限。
- 每轮采集 3 份独立有效样本。超时、流中断或样本不足时，对失败样本最多追加 2 次重试。
- 检测失败不会新增隔离；保留账号现有调度状态，稍后自动重试。
- 仅恢复插件确认持有且未被外部修改的暂停记录；连续两轮通过后恢复。
- 支持 OpenAI OAuth；安装可选本机桥接后，也支持 API Key 账号的 Responses API、模型映射及代理。
- 中文界面，实时展示排队、采样、重试、结果与下一步；设置和诊断按需展开。

**指纹表示参考库内的统计相似度，不是模型身份认证或智力证明。** 超时、拒答和不完整样本不会被当作指纹不符。

## 安装

从 [Releases](https://github.com/cyeinfpro/ModelGuard/releases) 下载 `.s2plugin` 和 `publisher-public-key.txt`。在 Sub2API 配置中添加可信发布者后，按宿主流程重载配置并上传插件：

```yaml
plugins:
  allow_unsigned: false
  trusted_publishers:
    cyeinfpro-account-guard-v1: "publisher-public-key.txt 中的公钥"
```

启用插件，打开“管理账号”，填写本站管理地址与管理员访问密钥。密钥需要 `accounts.read`、`accounts.status`，授权范围包含被测账号。选择账号、请求模型和预期指纹，保存后自动检测，也可点击“立即检测”。

首次使用建议先关闭“自动暂停调度”观察结果，再根据实际需求启用。正常间隔默认 30 分钟，失败重试或恢复确认间隔 2 分钟，隔离复测间隔 6 小时。保存配置后的启动保护期为 60 秒；手动检测可立即排队。

已验证宿主：官方 Sub2API 0.2.19。兼容声明为 `>=0.2.19 <0.3.0`，其他版本需自行验证。插件参与 `openai.oauth.outbound_transport.v1` 绑定，部署时需检查现有传输插件。持久化目录默认 `/app/data/sub2api-account-guard`。

为兼容既有安装，内部插件 ID 保留为 `local.sub2api.account-guard`，发布者 ID 与状态目录也保持兼容。

## API Key 账号

原版宿主不提供 API Key 类型的出站凭据。API 支持通过可选的本机 Unix Socket 助手实现，不修改 Sub2API 主程序，不开放 TCP 端口。助手对每次请求校验管理密钥的账号权限，然后只读获取该账号现有的上游地址、Key、模型映射和代理。

参见 [本机 API 桥接安装](docs/API-BRIDGE.md)。未安装桥接时，OAuth 正常工作，API 账号显示配置提示。

API 供应商需支持 `/v1/responses` SSE。检测聚合渠道时，隔离影响的是该渠道账号，无法单独识别渠道背后的供应商账号。API 账号默认请求 `gpt-6.1-sol`，预期指纹使用 Astra/Sol 类，可自行调整。

## 构建与测试

需要 Go 1.26.1 和 Node.js 22 或更新版本。Go 依赖已 vendored。

```sh
npm ci
make test
make build
```

构建输出包含 Linux amd64、arm64 运行文件和本机协议验证程序。打包需要仓库外的 Ed25519 签名种子：

```sh
go run ./cmd/package -key /secure/path/publisher.key -generate-key
# 后续版本使用同一个 key，不再传 -generate-key
make verify-package
```

自建签名包需把对应公钥加入宿主可信发布者。请勿上传私钥、管理密钥、桥接令牌或生产状态文件。官方发布包的公钥位于仓库根目录。

本地界面演示：运行 `python3 -m http.server 18971`，打开 `http://127.0.0.1:18971/tests/preview.html`，数据为模拟数据。

## 边界

- 不恢复外部暂停的账号；手动检测只能只读复测。插件持有的隔离记录会持久化。
- 暂停只影响新调度，不中断已经运行的请求。采样消耗上游账号额度，不进入普通 API Key 用量账单。
- 恢复前检查账号更新时间和状态，但宿主调度接口没有原子条件更新，检查与写入之间存在短暂竞态。避免多个自动控制器同时管理同一账号。
- 原始回答仅在内存参与评分，页面与持久化状态不保存凭据或原始回答。

## 来源与许可

项目以 LGPL-3.0 发布；第三方 MIT、MPL、Apache、BSD 组件保留各自许可。详见 [第三方声明](THIRD_PARTY_NOTICES.md)。指纹参考库来自 [Ikaleio/lm-detector](https://github.com/Ikaleio/lm-detector)，固定为 `a4ef212`，包含 64 类，来源与校验和见 [数据说明](internal/fingerprint/data/ORIGIN.md)。
