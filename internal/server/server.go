// Package server 是引擎的入站 HTTP 服务（**不经公网**：默认由进程内 cloudflared
// quick tunnel 暴露，见 internal/tunnel）。
//
//	POST /v1/tasks             受理派发（Bearer 引擎令牌）→ 入队 → 202
//	POST /v1/tasks/{id}/cancel 置取消标记（当前分片做完即停）
//	GET  /v1/healthz           版本/节点 id（无鉴权，隧道内可达即可）
//	POST /v1/update            自更新：排空 → 下载新版本 → exec 重生
//
// 这里只做 HTTP 映射与节点级状态（槽位、排水、在途计数）；下载本身在 internal/download。
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/atticus6/vultro-engine/internal/buildinfo"
	"github.com/atticus6/vultro-engine/internal/config"
	"github.com/atticus6/vultro-engine/internal/download"
	"github.com/atticus6/vultro-engine/internal/selfupdate"
)

// Server 入站 HTTP 服务。
type Server struct {
	cfg    *config.Config
	runner *download.Runner
	log    *slog.Logger
	slots  chan struct{}

	// draining 为真时**不再接受新任务**（收到更新指令后进入，见 handleUpdate）。
	draining atomic.Bool

	// inflight 已受理但还没跑完的任务数（含排队等槽位的）。
	// 排空判定必须用它，而不能只看 runner 的取消登记表：后者只登记"已经开始跑"的任务。
	inflight atomic.Int64

	// updateTarget 更新目标版本（空 = 最新），排空后由更新协程读取。
	updateTarget atomic.Value // string
}

// New 构造入站服务。槽位容量来自配置（每槽约 64 MiB 内存）。
func New(cfg *config.Config, runner *download.Runner, log *slog.Logger) *Server {
	return &Server{
		cfg:    cfg,
		runner: runner,
		log:    log,
		slots:  make(chan struct{}, cfg.Capacity),
	}
}

// Handler 返回挂好全部路由的 http.Handler（隧道指向它）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tasks", s.handleDispatch)
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", s.handleCancel)
	mux.HandleFunc("GET /v1/healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/update", s.handleUpdate)
	return mux
}

// Draining 是否正在排空（心跳把它上报给 Worker，Worker 据此暂停派发）。
func (s *Server) Draining() bool {
	return s.draining.Load()
}

// handleDispatch 受理派发：令牌校验 → 槽位排队 → goroutine 执行。
func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.cfg.Token {
		http.Error(w, `{"error":{"code":"UNAUTHORIZED","message":"令牌无效"}}`, http.StatusUnauthorized)
		return
	}
	var task download.Task
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
	if s.runner.IsRunning(task.TaskID) {
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
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.cfg.Token {
		http.Error(w, `{"error":{"code":"UNAUTHORIZED","message":"令牌无效"}}`, http.StatusUnauthorized)
		return
	}
	cancelled := s.runner.Cancel(r.PathValue("id"))
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]bool{"cancelled": cancelled})
}

// handleUpdate 自更新（Worker 后台「更新」按钮触发）。
//
// 请求体：`{ "version": "v1.2.3" }`，留空表示"最新 release"。
// 成功时本进程会被新二进制替换（不会返回 200——连接会在 exec 时断开），
// 所以响应只覆盖"开始更新/失败"两种情形；调用方按 202 处理即可。
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
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
	s.log.Info("收到更新指令，进入排空", "target", body.Version, "current", buildinfo.Version,
		"inflight", s.inflight.Load(), "alreadyDraining", already)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"accepted": true,
		"current":  buildinfo.Version,
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
		if err := selfupdate.SelfUpdate(ctx, target, s.log); err != nil {
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
func (s *Server) waitForIdle() bool {
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
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":  buildinfo.Version,
		"id":       s.cfg.ID,
		"draining": s.draining.Load(),
		"inflight": s.inflight.Load(),
	})
}
