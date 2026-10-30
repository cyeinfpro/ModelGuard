# 本机 API 凭据桥接

可选组件：仅在需要监测 API Key 账号时安装。它是独立的 Python 3 服务，通过 Unix Socket 提供指定账号凭据；每次读取前后均使用请求中的管理员密钥验证授权，数据库仅执行 SELECT。运行主机需要 Python 3、Docker CLI，以及访问 Sub2API PostgreSQL 容器的权限。

默认环境为 Docker 部署：宿主 `/root/sub2api-deploy/data` 映射到 Sub2API 容器 `/app/data`。请按实际部署调整路径、容器名和插件运行用户 UID/GID。桥接目录必须与插件持久化目录指向同一份宿主文件。

1. 将 `scripts/api-credential-bridge.py` 安装到 `/opt/modelguard/api-credential-bridge.py`。
2. 在共享状态目录生成随机令牌（不要将其写进仓库）：

```sh
sudo install -d -m 700 -o 1000 -g 1000 /root/sub2api-deploy/data/sub2api-account-guard
sudo python3 - <<'PY'
import os, secrets
path='/root/sub2api-deploy/data/sub2api-account-guard/api-bridge.token'
with os.fdopen(os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o400),'w') as f:
    f.write(secrets.token_hex(32))
os.chown(path,1000,1000)
PY
```

3. 创建 `/etc/systemd/system/modelguard-api-bridge.service`：

```ini
[Unit]
Description=ModelGuard API credential bridge
After=docker.service
Requires=docker.service
[Service]
ExecStart=/usr/bin/python3 /opt/modelguard/api-credential-bridge.py
Environment=MODEL_GUARD_DATA_DIR=/root/sub2api-deploy/data/sub2api-account-guard
Environment=MODEL_GUARD_DB_CONTAINER=sub2api-postgres
Environment=MODEL_GUARD_ADMIN_URL=http://127.0.0.1:8080
Environment=MODEL_GUARD_UID=1000
Environment=MODEL_GUARD_GID=1000
Restart=on-failure
RestartSec=3
UMask=0077
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/root/sub2api-deploy/data/sub2api-account-guard
[Install]
WantedBy=multi-user.target
```

4. 运行 `sudo systemctl daemon-reload` 与 `sudo systemctl enable --now modelguard-api-bridge`，确认服务正常后打开插件选择 API 账号。

Socket 为 `api-bridge.sock`，权限 0600。令牌仅供本机插件读取。不要公开 Socket、令牌或转发其接口到公网。

当前插件客户端要求 Sub2API 管理地址为 `http://127.0.0.1:8080` 或 `http://localhost:8080`。数据库用户与数据库名默认为 `sub2api`。不同部署拓扑需适配后验证；停止桥接后 API 检测阻塞并保留调度状态，OAuth 检测继续使用原生身份接口。


## 日常检查与故障处理

安装完成后先查看服务，再在 ModelGuard 的连接步骤加载账号：

```sh
systemctl status modelguard-api-bridge --no-pager
journalctl -u modelguard-api-bridge -n 30 --no-pager
```

| 页面或服务现象 | 含义 | 处理 |
| --- | --- | --- |
| 提示未安装桥接 | 插件状态目录找不到令牌或 socket | 核对容器映射和 MODEL_GUARD_DATA_DIR 是否指向同一份目录 |
| 有 socket 但请求失败 | 文件存在不代表服务仍在监听 | 查看 systemd 服务状态后重新启动服务 |
| HTTP 401 / 403 | 管理密钥无效，或没有该账号读取权限 | 更新管理密钥及账号授权范围，不要扩大到无关账号 |
| HTTP 409 | 两次授权读取之间，账号或调度状态发生变化 | 保留当前状态，稍后重试 |
| 暂时不可用 / 503 | 数据库读取、服务资源或请求处理暂时失败 | 查看桥接服务与 PostgreSQL 容器状态，插件不把此错误判成指纹不符 |
| 第二个实例启动失败 | 同一目录已有桥接进程 | 保留原服务，用 systemctl 管理，不同时手动启动多个进程 |

桥接只允许本机管理根地址，不跟随管理接口跳转，也不通过环境 HTTP 代理转发管理密钥。管理员访问密钥必须在读取凭据前后都拥有该账号的权限。成功响应禁止缓存，错误响应不包含凭据、原始异常或 SQL。

同一目录同时只运行一个桥接服务。启动时不会覆盖正在使用的 socket，也不会删除同名普通文件或符号链接；退出时只清理本进程创建的 socket。默认处理 8 个并行连接，另允许 16 个连接等待，超出时快速关闭，由插件显示暂时不可用并按既有策略重试。

源码验证可运行：

```sh
python3 -m unittest discover -s tests -p '*_test.py'
```

这些测试使用临时目录、本地假服务和模拟账号，不需要 Docker、真实管理密钥或生产数据库。
