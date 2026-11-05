# 本机 API 凭据桥接

仅在监测 API Key 账号时需要桥接。OAuth 账号使用宿主原生身份接口。

桥接只监听共享目录中的 Unix Socket。读取凭据前后，它使用请求携带的管理员密钥验证该账号的读取权限；数据库只提供 OpenAI API Key 账号的必要字段。新版默认通过 **专用只读角色直接连接 PostgreSQL**，运行进程无需 root，也不需要访问 Docker Socket。

## 连接与权限

| 项目 | 约定 |
| --- | --- |
| Socket | 插件状态目录中的 `api-bridge.sock`，0600 |
| 本地令牌 | 同目录 `api-bridge.token`，随机 64 位十六进制文本，0400；不得放进仓库 |
| 健康检查 | `GET /healthz`，需 `x-bridge-token`，仅返回 `{"ok":true}`，不读取账号、管理员密钥或数据库 |
| 账号读取 | `GET /accounts/<id>`，同时要求本地令牌与该账号的管理员读取权限 |
| 管理地址 | HTTP/HTTPS 回环主机，支持 localhost、127.0.0.0/8、IPv6 ::1 和任意有效端口；不允许路径、用户信息、查询或跳转 |
| 数据库 | 专用 `modelguard_bridge` 角色，只能 SELECT 投影视图，无基础表读取或写入权限 |
| 连接复用 | 最多 8 条连接；只读事务、5 秒连接超时、8 秒语句超时 |
| 响应 | 禁止缓存；错误不包含 SQL、密钥、数据库密码或原始异常 |

`/healthz` 证明进程正在响应，不代表管理员授权或上游模型请求已经成功。两类状态在插件中分别显示。

## 首次准备共享目录

已有桥接迁移时保留现有令牌。新安装时在与插件相同的状态目录生成一次随机令牌，目录 0700、令牌 0400，所有者均为插件运行 UID/GID。以下示例不会覆盖已有令牌，也不输出令牌：

```sh
sudo install -d -m 700 -o 1000 -g 1000 /root/sub2api-deploy/data/sub2api-account-guard
sudo python3 - <<'PYTOKEN'
import os, secrets
path = '/root/sub2api-deploy/data/sub2api-account-guard/api-bridge.token'
with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o400), 'w') as file:
    file.write(secrets.token_hex(32))
os.chown(path, 1000, 1000)
PYTOKEN
```

## 1. 配置只读数据库入口

安装包携带 `scripts/api-bridge-readonly.sql`。以数据库管理角色执行一次。该脚本创建或更新专用角色、设置密码和只读默认值，并创建 `public.modelguard_bridge_accounts` 视图。

视图只投影账号状态、更新时间、过期时间，以及 `api_key`、`base_url`、`model_mapping` 和关联代理必需字段。它不暴露 OAuth 凭据、完整 credentials JSON、已删除账号或子账号。桥接角色没有基础表权限，不能通过绕过桥接查询其他凭据。

执行时传入 `bridge_role` 和 `db_name`；密码通过当前进程的 `MODEL_GUARD_DB_PASSWORD` 环境变量传入，不作为命令参数，不打印：

```sh
psql -X -v ON_ERROR_STOP=1 -v bridge_role=modelguard_bridge \
  -v db_name=sub2api -f scripts/api-bridge-readonly.sql
```

在现有 Docker 数据库中，可由部署管理员通过一次性的 `docker exec -i` 执行该 SQL。Docker 权限只用于部署，不授予运行中的桥接进程。

用 libpq passfile 保存同一个密码，权限 0400，所有者为桥接运行 UID。格式为：

```text
数据库主机:5432:sub2api:modelguard_bridge:数据库密码
```

密码中的冒号与反斜杠需要按 libpq passfile 规则转义。不要把密码写入命令行、systemd Environment、日志或镜像层。环境配置只保存 passfile 的路径。

部署后验证该角色能够查询视图，同时对 `accounts`、`proxies` 的直接读取与任意 UPDATE 均被拒绝。不要用真实账号修改来验证权限。

## 2. 选择运行方式

### 已有 Docker 部署：非 root 伴随容器

这适用于 PostgreSQL 未向宿主发布端口的现有部署，不需要重建数据库或临时开放数据库端口。

使用安装包内的 Dockerfile 构建桥接镜像：

```sh
docker build -t modelguard-api-bridge:local -f scripts/api-bridge.Dockerfile scripts
```

桥接与 Sub2API 共享网络命名空间，因此可使用回环管理地址，也可通过现有 Docker 网络的数据库名称连接。只挂载插件状态目录和私有 passfile：

```sh
docker run -d --name modelguard-api-bridge \
  --network container:sub2api --restart unless-stopped \
  --user 1000:1000 --cap-drop ALL --security-opt no-new-privileges \
  --read-only --tmpfs /tmp:rw,noexec,nosuid,size=16m \
  --mount type=bind,src=/root/sub2api-deploy/data/sub2api-account-guard,dst=/var/lib/modelguard \
  --mount type=bind,src=/etc/modelguard/pgpass,dst=/etc/modelguard/pgpass,readonly \
  -e MODEL_GUARD_ADMIN_URL=http://127.0.0.1:8080 \
  -e MODEL_GUARD_DB_HOST=sub2api-postgres \
  -e MODEL_GUARD_UID=1000 -e MODEL_GUARD_GID=1000 \
  modelguard-api-bridge:local
```

根据真实容器名、UID/GID、目录和管理端口调整。插件与桥接必须看到同一份状态目录，并以同一 UID 创建 socket 和读取令牌。不要挂载 Docker Socket，不要使用 privileged，也不要发布桥接端口。

Sub2API 被重新创建时，同时重建桥接容器，让它加入新的网络命名空间。生产部署建议把以下服务加入原来的 Compose 文件，或加入每次操作都同时使用的 override 文件：

```yaml
services:
  modelguard-api-bridge:
    image: modelguard-api-bridge:local
    network_mode: service:sub2api
    depends_on:
      sub2api:
        condition: service_started
        restart: true
    restart: unless-stopped
    user: "1000:1000"
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    read_only: true
    tmpfs:
      - /tmp:rw,noexec,nosuid,size=16m
    volumes:
      - /root/sub2api-deploy/data/sub2api-account-guard:/var/lib/modelguard
      - /etc/modelguard/pgpass:/etc/modelguard/pgpass:ro
    environment:
      MODEL_GUARD_ADMIN_URL: http://127.0.0.1:8080
      MODEL_GUARD_DB_HOST: sub2api-postgres
      MODEL_GUARD_UID: "1000"
      MODEL_GUARD_GID: "1000"
```

对已运行的宿主，可仅执行 `docker compose up -d --no-deps modelguard-api-bridge` 启动桥接，不重建宿主或数据库。以后升级宿主时，完整 Compose 配置必须包含此服务，才能让依赖重启规则生效；若使用显式 `-f` 参数，也要带上桥接所在的 override 文件。不要把数据库容器的临时 IP 写成长期配置。

### 数据库已有 Unix Socket 或固定本机入口：systemd

主机安装 Python 3 与 psycopg 3（Debian 可用 `python3-psycopg`），将脚本安装为 `/opt/modelguard/api-credential-bridge.py`。使用安装包里的 `scripts/modelguard-api-bridge.service`。

服务以非 root UID/GID 运行，应与插件目录所有者相同。模板使用 UID/GID 1000，并通过 `BindPaths` 把既有目录映射到服务命名空间的 `/var/lib/modelguard-bridge`，无需放开 `/root` 权限。按实际路径修改模板，创建目标挂载目录。

在 `/etc/modelguard/api-bridge.env` 中保存无秘密配置：

```ini
MODEL_GUARD_DB_MODE=direct
MODEL_GUARD_DB_HOST=/var/run/postgresql
MODEL_GUARD_DB_PORT=5432
MODEL_GUARD_DB_NAME=sub2api
MODEL_GUARD_DB_USER=modelguard_bridge
MODEL_GUARD_DB_PASSFILE=/etc/modelguard/pgpass
MODEL_GUARD_ADMIN_URL=http://127.0.0.1:8080
MODEL_GUARD_UID=1000
MODEL_GUARD_GID=1000
```

`MODEL_GUARD_DB_SSLMODE` 默认 prefer；远程数据库应按部署证书要求使用 verify-full。运行服务无 Docker 依赖，systemd 模板包含空 CapabilityBoundingSet、NoNewPrivileges、PrivateTmp、ProtectSystem 和网络地址族限制。

检查模板路径、目录和 passfile 权限后，再执行 daemon-reload 与启用服务。一次只运行一个桥接实例。

## 3. 从旧 root / docker-exec 版本迁移

1. 备份现有服务配置，保留原 socket 令牌和插件 state.json。
2. 用数据库管理员建立只读视图和角色，生成私有 passfile。
3. 验证新角色只能读取视图；安装 psycopg 或构建伴随容器镜像。
4. 停止旧桥接，启动新的非 root 进程；不要并行占用同一个 socket。
5. 通过带本地令牌的 `/healthz` 检查在线状态，再在插件内重新加载账号。
6. 保留现有监测目标、自动暂停/恢复开关及预算。此次安装步骤不发起真实模型请求。

默认 direct 模式不会自动降级到 root 或 Docker。为迁移排障保留显式 `MODEL_GUARD_DB_MODE=docker`，仍使用只读视图，数据库用户名和库名分别从 `MODEL_GUARD_DB_USER`、`MODEL_GUARD_DB_NAME` 读取。该模式仍需要 Docker 权限，应仅作为短期回退方案；确认直连正常后移除旧服务和 Docker 授权。

停止桥接只会令 API Key 检测暂时不可用；已有调度状态不会因此自动改变，OAuth 检测仍使用宿主身份接口。

## 日常检查

| 现象 | 含义与处理 |
| --- | --- |
| 未找到令牌或 socket | 核对宿主与插件容器的状态目录映射和 UID/GID |
| socket 存在但健康检查失败 | 服务未监听、令牌不一致或权限不符；查看服务状态 |
| 健康检查通过，账号读取 401/403 | 管理员密钥没有该账号的读取权限 |
| 账号读取 409 | 前后授权快照或调度状态发生变化，稍后重试 |
| 503 | 只读数据库入口、驱动、passfile、连接数或权限异常；不属于指纹不符 |
| 第二个实例启动失败 | 原服务仍在运行，使用服务管理器停止/启动，不覆盖 socket |
| 宿主重新创建后 API 检测失败 | 重建共享其网络命名空间的桥接容器 |

服务不会覆盖正在使用的 socket，不会删除同名普通文件或符号链接，退出只清理本进程创建的 socket。默认 8 个处理线程和 16 个排队连接，超出时快速关闭。管理员密钥不跟随跳转，也不通过环境 HTTP 代理发送。

## 合成验证

```sh
python3 -m unittest discover -s tests -p '*_test.py'
```

测试使用 /tmp 短路径、本地假服务和模拟数据库，不访问生产账号，不需要 Docker 或真实凭据。仅在文件系统明确报告不支持 Unix Socket 时跳过相关测试；权限拒绝和逻辑错误仍会失败。
