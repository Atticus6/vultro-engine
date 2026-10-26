# Vultro 离线下载引擎（Go）

服务端代下载的执行端：接收 Worker 派发的任务，从源站拉取字节，按分片回传 Worker
（写入用户的上传会话）。多实例部署，每个实例一条 cloudflared named tunnel。

架构与契约见 `docs/离线下载设计方案.md`（§2 总体链路、§6.3 派发载荷、§7 本引擎）。

## 职责边界（引擎刻意很薄）

- **不持有 S3 密钥、不连 D1**：一切存储与元数据操作都在 Worker 侧；
- **不监听公网**：入站只有 cloudflared 隧道可达的 `/v1/*` 三个端点；
- **对去重零感知**：看到的永远是普通单会话下载任务；搭车/结算/归零全是 Worker 侧编排；
- **无本地状态**：断点续传靠派发载荷里的 `uploadedParts`（Worker 从 R2 `ListParts` 取），
  引擎崩溃/改派零恢复成本。

## 环境变量

| 变量                | 必填 | 说明                                                                              |
| ------------------- | ---- | --------------------------------------------------------------------------------- |
| `ENGINE_ID`         | ✅   | 引擎 id（如 `off1`），与隧道主机名一一对应，Worker 侧派发路由键                   |
| `ENGINE_PUBLIC_URL` | ✅   | 隧道地址（`https://off1.vultro.app`）；Worker 向它 `POST /v1/tasks`               |
| `ENGINE_TOKEN`      | ✅   | 引擎令牌（与 Worker 的 `OFFLINE_ENGINE_TOKEN` 同值；双向 Bearer）                 |
| `WORKER_BASE_URL`   | ✅   | Worker 基址（`https://www.vultro.app` / 测试环境 / 本地 `http://localhost:8787`） |
| `ENGINE_CAPACITY`   |      | 并发槽位（默认 2，1–16；每槽一片 64 MiB 内存缓冲）                                |
| `ENGINE_ADDR`       |      | 本地监听（默认 `:8787`，只给隧道/本机）                                           |
| `EGRESS_MODE`       |      | `relay`（默认，源站流量经 Worker 出口）/ `direct`（直连源站）                     |
| `EGRESS_PROXY`      |      | direct 模式的 http/https 上游代理（如 WARP）；socks5 暂不支持                     |

## 构建与本地运行

```sh
go build -o engine .        # 产物单二进制
go test ./...               # 单测
```

本地联调（Worker 用 `bun run dev` 跑在 8787，免隧道直连）：

```sh
ENGINE_ID=off1 \
ENGINE_PUBLIC_URL=http://localhost:8788 \
ENGINE_TOKEN=<与 Worker .env 的 OFFLINE_ENGINE_TOKEN 同值> \
WORKER_BASE_URL=http://localhost:8787 \
ENGINE_ADDR=:8788 \
./engine
```

## 生产部署（cloudflared named tunnel）

一机一引擎一隧道：

```sh
# 1. 隧道（首次）
cloudflared tunnel create off1
cloudflared tunnel route dns off1 off1.vultro.app

# 2. ingress（~/.cloudflared/config.yml）
#   tunnel: <off1 的 UUID>
#   credentials-file: /etc/cloudflared/off1.json
#   ingress:
#     - hostname: off1.vultro.app
#       service: http://localhost:8787
#     - service: http_status:404

# 3. systemd 两个服务：cloudflared（隧道）+ vultro-engine（本二进制 + 上表环境变量）
```

**出口与 IP 暴露（如实说明）**：cloudflared 是**入站**隧道，不提供通用出口。
默认 `EGRESS_MODE=relay` 下，引擎对源站的每次请求都经 Worker 的 relay 端点转发，
源站只见 Cloudflare 出口 IP——这正是默认推荐。`direct` 模式（源站封锁 CF 出口时）
建议主机安装 WARP（wgcf）或配置 `EGRESS_PROXY`，否则源站会看到主机 IP。

## 端点契约（Worker → 引擎，Bearer 引擎令牌）

| 方法 | 路径                    | 说明                                             |
| ---- | ----------------------- | ------------------------------------------------ |
| POST | `/v1/tasks`             | 受理派发载荷（见方案 §6.3）→ 202；同 id 重放幂等 |
| POST | `/v1/tasks/{id}/cancel` | 置取消标记，当前分片做完即停                     |
| GET  | `/v1/healthz`           | `{version, id}`（无鉴权，隧道内可达即可）        |

执行循环：逐片 `Range` 取源站（relay 经 Worker / direct 直连）→ 内存一片（≤64 MiB）→
`PUT /api/upload/{sessionId}/part/{n}`（任务令牌）→ 节流报进度 → 全部完成后
`POST /api/offline/tasks/{id}/finish`（成功 → Worker 结算扇出；失败 → abort + 传导）。

## 版本

构建时注入：`go build -ldflags "-X main.Version=v1.0.0"`。
注册/心跳上报给 Worker（`offline_engine.version`），滚动升级时用于判断哪台还在跑旧版。
