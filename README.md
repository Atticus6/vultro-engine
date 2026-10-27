# Vultro 离线下载引擎

服务端代下载的执行端：接收 Worker 派发的任务，从源站拉取字节，按分片回传 Worker
（写入用户的上传会话）。可多实例部署，每个实例一条 cloudflared named tunnel。

- 仓库：<https://github.com/atticus6/vultro-engine>
- 许可：MIT
- 形态：单静态二进制（裸机/VM）+ 多架构镜像 `ghcr.io/atticus6/vultro-engine`

## 它是怎么工作的

```
引擎启动 ──内嵌 cloudflared──▶ https://xxx.trycloudflare.com（随机地址）
   └── 注册（上报该地址）──▶ Worker（后台「离线节点」里可见）

Worker ──POST /v1/tasks（引擎令牌，经隧道）──▶ 引擎
                                               │
   源站 ◀── warp：用户态 WireGuard 出网 ───────┤ 逐片 Range GET → 内存一片（≤64 MiB）
          （或 relay：经 Worker 转发）          │
Worker ◀──PUT /api/upload/{sid}/part/{n} ──────┘ 分片回传（任务令牌）
Worker ◀──POST /api/offline/tasks/{id}/finish─── 成功则 Worker 自建清单核对并 complete
```

职责边界（引擎刻意很薄）：

- **不持有 S3 密钥、不连数据库**：存储与元数据全在 Worker；
- **不监听公网、不需要公网 IP**：入站几乎立刻变成一条**进程内 cloudflared 临时隧道**
  （`*.trycloudflare.com`），启动后自动把地址注册给 Worker；
- **别把宿主机 IP 暴露给源站**：默认 `EGRESS_MODE=warp`，取源站走用户态 WireGuard（WARP），
  源站看到的是 WARP 出口 IP；
- **对「同一 URL 去重」零感知**：看到的永远是普通单会话下载任务；
- **无本地状态**：断点续传靠派发载荷里的 `uploadedParts`（Worker 从对象存储 `ListParts` 取），
  进程崩溃或任务改派都不需要恢复步骤。

## 安装（裸机/VM）

```sh
curl -fsSL https://raw.githubusercontent.com/atticus6/vultro-engine/main/install.sh | sh
```

脚本按 OS/架构下载最新 release、校验 checksums、装到 `/usr/local/bin`
（无写权限时落到 `~/.local/bin`）。可用 `--version v1.0.0` 固定版本。

## 安装（Docker Compose，推荐）

```sh
git clone https://github.com/atticus6/vultro-engine.git
cd vultro-engine
cp .env.example .env      # 填 ENGINE_TOKEN（后台创建节点时给出的一次性令牌）；其余有默认值
docker compose up -d
```

**只有一个容器**：cloudflared 已内嵌，引擎自己开临时隧道并上报地址——不需要创建
tunnel、不需要配 DNS、不需要公网 IP。启动后到后台「离线节点」页确认该节点变成「在线」。

### 令牌从哪来（每节点一枚）

后台「离线下载节点」页 → **添加节点**（填 id 与槽位）→ 页面给出该节点的 `eng_…` 令牌与
填好的启动片段。**令牌只显示那一次**：丢了就到该行点「重置令牌」，把新令牌写进
`ENGINE_TOKEN` 并重启引擎。

没有全局共享的引擎 secret——每台机器一枚，一台泄露不影响其它节点；重置只吊销那一台
（旧令牌立即失效）。

需要放行的**出站**端口：TCP 443（Cloudflare 边缘）、TCP 7844（隧道，HTTP/2）；
`EGRESS_MODE=warp` 还需要 UDP 2408（WARP）。**不需要任何入站端口。**

## 环境变量

| 变量                | 必填 | 说明                                                             |
| ------------------- | ---- | ---------------------------------------------------------------- |
| `ENGINE_ID`         | ✅   | 引擎 id（如 `off1`），仅用于本地日志与运维对账（身份由令牌决定） |
| `ENGINE_TOKEN`      | ✅   | 该节点专属的 `eng_…`（双向 Bearer；后台创建/重置节点时给出）     |
| `WORKER_BASE_URL`   | ✅   | Worker 基址（测试/生产各自的域名）                               |
| `ENGINE_PUBLIC_URL` |      | 留空 = 内嵌临时隧道并自动上报；填了则用外部隧道（不再内嵌）      |
| `ENGINE_CAPACITY`   |      | 并发槽位（默认 2，1–16；每槽约 64 MiB 内存）                     |
| `ENGINE_ADDR`       |      | 本地监听（默认 `:8787`）                                         |
| `EGRESS_MODE`       |      | `relay`（默认）/ `warp` / `direct`                               |
| `EGRESS_PROXY`      |      | 仅 direct：http/https 上游代理（socks5 不支持）                  |

## 入站端点（Worker → 引擎，Bearer 该节点令牌）

| 方法 | 路径                    | 说明                                      |
| ---- | ----------------------- | ----------------------------------------- |
| POST | `/v1/tasks`             | 受理派发载荷 → 202；同 id 重放幂等        |
| POST | `/v1/tasks/{id}/cancel` | 置取消标记，当前分片做完即停              |
| GET  | `/v1/healthz`           | `{version, id}`（无鉴权，隧道内可达即可） |

## 安全模型

- **两把钥匙**：引擎令牌（长期共享，认节点）与任务令牌（每任务一枚、短时效 HMAC，
  认作业）。前者只在部署配置里，后者由 Worker 派发时下发、随任务过期失效。
  一枚泄漏的任务令牌 = 一个作业的一个会话 + 一条源 URL，而不是整个系统。
- **relay 不是开放代理**：Worker 的 fetch 端点要求请求 URL 与任务登记的源地址**逐字符相等**。
- **出口与 IP**：临时隧道只解决**入站**（Worker → 引擎）。源站方向由 `EGRESS_MODE`
  决定：`warp`（默认）经用户态 WireGuard 出网，源站只看到 WARP IP；`relay` 经 Worker
  转发，源站看到 Cloudflare IP；`direct` 会暴露宿主 IP。**`warp` 握手失败时进程直接退出**，
  不会静默退回直连——那会把宿主 IP 漏给源站。

## 自更新（后台一键升级）

后台「离线节点」里每个落后版本的节点都有「更新」按钮，它调用引擎的
`POST /v1/update`（引擎令牌）。引擎的执行顺序是：

1. **进入排水**：立刻不再接受新任务（`/v1/tasks` 返回 503，心跳上报
   `draining=true`，Worker 随即把它从派发候选里排除）；
2. **等在途任务跑完**：最长 60 分钟；超时则**取消本次更新并恢复接单**，
   节点不会永久卡在排水状态；
3. 下载目标版本 release + `checksums.txt`，校验 SHA-256；
4. 解出二进制，写到**同目录**临时文件，先用 `新二进制 --version` 冒烟检查；
5. `rename` 原子覆盖，再 `syscall.Exec` 原地把进程换成新版本——隧道会重建、
   地址会变，启动流程会把新地址重新注册给 Worker。

要点：

- 任何一步失败都**不会**覆盖现有二进制；更新失败会自动恢复接单；
- 需要二进制所在目录**可写**（`/usr/local/bin` 通常要 root）。容器里升级更稳的做法
  是让镜像 tag 跟随版本、交给编排更新；
- 也可手动触发：`curl -X POST -H "authorization: Bearer $ENGINE_TOKEN" \
-d '{"version":"v1.0.0"}' http://127.0.0.1:8787/v1/update`（留空版本 = 最新）。
  `$ENGINE_TOKEN` 是**本节点**的令牌（`eng_…`），不是任何全局 secret。

## 已知限制（重要）

- **临时隧道由 Cloudflare 标注为"仅供测试"，没有 SLA**，地址每次启动都会变化
  （引擎会重新注册，Worker 侧延迟最多一个注册周期）；
- 内嵌走的是 cloudflared 的 CLI 命令图：cloudflared 没有公开的 quick tunnel SDK
  （[#986](https://github.com/cloudflare/cloudflared/issues/986)），因此升级 cloudflared
  依赖后需要跑一次 `go test` 并实际连一次隧道；
- 需要出站 UDP/TCP 被放行；QUIC 在多数机房不可用，默认走 HTTP/2。

## 从源码构建与测试

```sh
go build -o vultro-engine . && go test ./...
```

版本号注入：`go build -ldflags "-X main.Version=v1.0.0"`（CI 已自动处理）。

## 发布

打 `v*` 标签触发 GitHub Actions（`.github/workflows/release.yml`）：

- 四个平台（linux/darwin × amd64/arm64）的归档 + `checksums.txt`（GoReleaser）；
- 多架构镜像 `ghcr.io/atticus6/vultro-engine:latest` 与 `:<version>`。

```sh
git tag v1.0.0 && git push origin v1.0.0
```

## 从主仓（monorepo）同步

本目录是独立仓库的源：主仓里改动后，用 `git subtree` 推过去：

```sh
git subtree push --prefix apps/engine git@github.com:atticus6/vultro-engine.git main
```
