// 引擎进程入口与本地 HTTP 服务：受理 Worker 的派发、暴露健康检查。
//
// 入站端点（**不经公网**：默认由进程内 cloudflared quick tunnel 暴露，见 tunnel.go）：
//
//	POST /v1/tasks             受理派发（Bearer 引擎令牌）→ 入队 → 202
//	POST /v1/tasks/{id}/cancel 置取消标记（当前分片做完即停）
//	GET  /v1/healthz           版本/节点 id（无鉴权，隧道内可达即可）
//
// 出站：
//   - 注册/心跳/进度/finish/relay 取流 → Worker（普通 HTTPS，见 worker.go）；
//   - 取源站字节 → 取决于 EGRESS_MODE：relay（经 Worker）/ warp（用户态 WireGuard）/
//     direct（直连或上游代理，见 warp.go 与 download.go 的 fetchRange）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"
)

func main() {
	// `--version` 必须在读配置之前处理：自更新会用 `新二进制 --version` 做冒烟检查，
	// 这一步不能因为缺环境变量而失败。
	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-version" || arg == "-v" {
			fmt.Println(Version)
			return
		}
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := LoadConfig()
	if err != nil {
		logger.Error("配置加载失败", "err", err)
		os.Exit(1)
	}

	// ── 1) 先起本地服务：隧道要指向它 ─────────────────────────────────────────
	worker := NewWorkerClient(cfg.WorkerBaseURL, cfg.Token, nil, logger)

	sourceClient, closeEgress, err := buildEgress(cfg, logger)
	if err != nil {
		logger.Error("出口初始化失败（拒绝以会暴露宿主机 IP 的方式启动）", "err", err)
		os.Exit(1)
	}
	defer closeEgress()

	runner := NewRunner(worker, sourceClient, cfg.EgressMode, logger)
	srv := &server{
		cfg:    cfg,
		runner: runner,
		log:    logger,
		slots:  make(chan struct{}, cfg.Capacity),
	}
	httpServer := &http.Server{Addr: cfg.Addr, Handler: srv.mux()}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP 服务退出", "err", err)
			os.Exit(1)
		}
	}()
	localURL := "http://127.0.0.1" + normalizeAddr(cfg.Addr)

	// ── 2) 解析公网地址：显式配置 或 内嵌临时隧道 ────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	publicURL := cfg.PublicURL
	if publicURL == "" {
		logger.Info("未配置 ENGINE_PUBLIC_URL，启动内嵌 cloudflared 临时隧道",
			"protocol", cfg.TunnelProtocol)
		publicURL, err = StartQuickTunnel(ctx, localURL, cfg.TunnelProtocol, logger)
		if err != nil {
			logger.Error("临时隧道启动失败", "err", err)
			os.Exit(1)
		}
		// quick tunnel 仅供测试/自托管：地址每次启动变化，注册时上报给 Worker 即可。
		logger.Warn("已使用临时隧道（*.trycloudflare.com）：Cloudflare 标注其不适合生产、无 SLA")
	}
	cfg.PublicURL = publicURL

	// ── 3) 注册（幂等）：把**当前**隧道地址报给 Worker ────────────────────────
	for {
		regCtx, regCancel := context.WithTimeout(ctx, 15*time.Second)
		err := worker.Register(regCtx, cfg)
		regCancel()
		if err == nil {
			logger.Info("已注册到 Worker", "id", cfg.ID, "publicUrl", cfg.PublicURL)
			break
		}
		logger.Warn("注册失败，10s 后重试", "err", err)
		select {
		case <-ctx.Done():
			os.Exit(1)
		case <-time.After(10 * time.Second):
		}
	}

	// ── 4) 心跳：刷新 lastSeenAt；Worker 有排队任务时会立即扫一轮派发 ─────────
	go func() {
		for {
			hbCtx, hbCancel := context.WithTimeout(ctx, 15*time.Second)
			hint, err := worker.Heartbeat(hbCtx, cfg.ID, Version, srv.draining.Load())
			hbCancel()
			if err != nil {
				logger.Warn("心跳失败", "err", err)
			} else if hint {
				logger.Info("有排队任务待派发")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}()

	logger.Info("引擎启动",
		"addr", cfg.Addr, "capacity", cfg.Capacity, "egress", cfg.EgressMode,
		"publicUrl", cfg.PublicURL, "version", Version)

	// 主 goroutine 阻塞：隧道/HTTP 服务的 fatal 分支都会走 os.Exit。
	select {}
}

// buildEgress 按 EGRESS_MODE 构造"取源站"的 HTTP 客户端。
//
// 返回的 close 函数负责释放资源（目前只有 WARP 的设备）。
// **warp 模式的失败必须冒泡**：静默退回直连会让源站看到宿主机 IP，正好违背该模式的
// 唯一目的——所以这里的错误让进程直接退出。
func buildEgress(cfg *Config, logger *slog.Logger) (*http.Client, func(), error) {
	noop := func() {}

	switch cfg.EgressMode {
	case "warp":
		egress, err := StartWarpEgress(cfg.WarpIdentityPath, logger)
		if err != nil {
			return nil, noop, err
		}
		return egress.HTTPClient(10 * time.Minute), egress.Close, nil

	case "direct":
		client := &http.Client{Timeout: 10 * time.Minute}
		if cfg.EgressProxy != "" {
			transport, err := proxyTransport(cfg.EgressProxy)
			if err != nil {
				return nil, noop, err
			}
			client.Transport = transport
		}
		return client, noop, nil

	default: // relay
		// relay 模式下取流走 Worker 的 fetch 端点，这个客户端不会用到；给个零值即可。
		return &http.Client{Timeout: 10 * time.Minute}, noop, nil
	}
}

// proxyTransport 为 direct 模式构造带代理的 Transport（支持 http/https 上游）。
func proxyTransport(raw string) (*http.Transport, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch parsed.Scheme {
	case "http", "https":
		return &http.Transport{Proxy: http.ProxyURL(parsed)}, nil
	default:
		// 标准库不带 socks5 支持（不引第三方依赖）；用 http 上游代理或 EGRESS_MODE=warp。
		return nil, fmt.Errorf("不支持的代理协议 %q（v1 支持 http/https 上游）", parsed.Scheme)
	}
}

// normalizeAddr 把监听地址转成 URL 里的 host:port（`:8787` → `:8787`，`0.0.0.0:8787` → `:8787`）。
//
// 隧道指向的是**本机回环**：监听在 0.0.0.0 时用 127.0.0.1 访问更稳（某些环境里
// 0.0.0.0 不是可拨号地址）。
func normalizeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return ":" + port
	}
	return addr
}

// server 入站 HTTP 服务（仅隧道可达）。
type server struct {
	cfg    *Config
	runner *Runner
	log    *slog.Logger
	slots  chan struct{}

	// draining 为真时**不再接受新任务**（收到更新指令后进入，见 handleUpdate）。
	draining atomic.Bool

	// inflight 已受理但还没跑完的任务数（含排队等槽位的）。
	// 排空判定必须用它，而不能只看 runner.cancels：后者只登记"已经开始跑"的任务。
	inflight atomic.Int64

	// updateTarget 更新目标版本（空 = 最新），排空后由更新协程读取。
	updateTarget atomic.Value // string
}

func (s *server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tasks", s.handleDispatch)
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", s.handleCancel)
	mux.HandleFunc("GET /v1/healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/update", s.handleUpdate)
	return mux
}

// handleDispatch 受理派发：令牌校验 → 槽位排队 → goroutine 执行。
func (s *server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.cfg.Token {
		http.Error(w, `{"error":{"code":"UNAUTHORIZED","message":"令牌无效"}}`, http.StatusUnauthorized)
		return
	}
	var task DownloadTask
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&task); err != nil {
		http.Error(w, `{"error":{"code":"BAD_REQUEST","message":"载荷无法解析"}}`, http.StatusBadRequest)
		return
	}
	if task.TaskID == "" || task.TaskToken == "" || task.Source.URL == "" {
		http.Error(w, `{"error":{"code":"BAD_REQUEST","message":"载荷字段缺失"}}`, http.StatusBadRequest)
		return
	}

	// 排水中（正在等任务跑完再更新）：明确拒绝，Worker 会把任务退回队列并改派。
	if s.draining.Load() {
		w.Header().Set("Retry-After", "30")
		http.Error(w, `{"error":{"code":"DRAINING","message":"节点正在排空以升级，暂不接新任务"}}`, http.StatusServiceUnavailable)
		return
	}

	// 同 id 重复派发（重放）：幂等 202，不再入队——重复执行会重复上传同一批分片。
	if _, running := s.runner.cancels.lookup(task.TaskID); running {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]bool{"accepted": true, "duplicate": true})
		return
	}

	// 槽位满时也先 202：任务入等待队列（goroutine 阻塞在 slots 上），
	// Worker 侧无需感知引擎的排队深度（capacity/inFlight 已表达）。
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Add(-1)
		s.slots <- struct{}{}
		defer func() { <-s.slots }()
		s.runner.Run(context.Background(), task)
	}()

	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]bool{"accepted": true})
}

// handleCancel 置取消标记：当前分片做完即停（≤1 片的浪费）。
func (s *server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.cfg.Token {
		http.Error(w, `{"error":{"code":"UNAUTHORIZED","message":"令牌无效"}}`, http.StatusUnauthorized)
		return
	}
	cancelled := s.runner.cancels.cancel(r.PathValue("id"))
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]bool{"cancelled": cancelled})
}

// handleUpdate 自更新（Worker 后台「更新」按钮触发）。
//
// 请求体：`{ "version": "v1.2.3" }`，留空表示"最新 release"。
// 成功时本进程会被新二进制替换（不会返回 200——连接会在 exec 时断开），
// 所以响应只覆盖"开始更新/失败"两种情形；调用方按 202 处理即可。
func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.cfg.Token {
		http.Error(w, `{"error":{"code":"UNAUTHORIZED","message":"令牌无效"}}`, http.StatusUnauthorized)
		return
	}
	var body struct {
		Version string `json:"version"`
	}
	// 空体也合法（= 最新版本）。
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			http.Error(w, `{"error":{"code":"BAD_REQUEST","message":"载荷无法解析"}}`, http.StatusBadRequest)
			return
		}
	}

	s.updateTarget.Store(body.Version)
	already := s.draining.Swap(true)
	s.log.Info("收到更新指令，进入排空", "target", body.Version, "current", Version,
		"inflight", s.inflight.Load(), "alreadyDraining", already)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"accepted": true,
		"current":  Version,
		"draining": true,
		"inflight": s.inflight.Load(),
	})

	if already {
		return // 已在排空（重复指令），等那个协程收尾即可
	}

	// 排空 → 更新。响应已经发出去：下载/校验可能要几十秒，exec 还会断开连接。
	go func() {
		target, _ := s.updateTarget.Load().(string)
		if !s.waitForIdle() {
			// 超时：放弃本次更新并恢复接单，避免节点永久处于排水状态。
			s.draining.Store(false)
			s.log.Error("排空超时，已取消本次更新并恢复接单", "inflight", s.inflight.Load())
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		if err := SelfUpdate(ctx, target, s.log); err != nil {
			// 更新失败：恢复接单（当前版本继续工作），后台再点一次即可。
			s.draining.Store(false)
			s.log.Error("自更新失败，已恢复接单", "err", err)
		}
	}()
}

// drainTimeout 排空的最长等待时间。
//
// 为什么要有上限：任务可能长时间卡在慢源上；无限等待会让节点永远排水（等于从集群里
// 消失）。超时后 **取消本次更新并恢复接单**，由管理员稍后重试——比强行杀掉任务好。
const drainTimeout = 60 * time.Minute

// waitForIdle 等到在途任务归零；超时返回 false。
func (s *server) waitForIdle() bool {
	deadline := time.Now().Add(drainTimeout)
	for time.Now().Before(deadline) {
		if s.inflight.Load() == 0 {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// handleHealth 版本与节点 id（无鉴权：隧道内可达即可）。
func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":  Version,
		"id":       s.cfg.ID,
		"draining": s.draining.Load(),
		"inflight": s.inflight.Load(),
	})
}
