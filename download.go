// 下载执行器：单个任务的下载循环（源站 Range GET → 内存一片 → 分片上传 Worker）。
//
// v1 不落盘：每片 ≤64 MiB 缓冲在内存（与 Worker 侧分片上限一致），下载完一片传一片。
// 断点续传不需要引擎侧任何持久化：改派时 Worker 在派发载荷里给出已传分片号
// （来自 R2 ListParts），跳过即可。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// DownloadTask 派发载荷（Worker → 引擎，`POST /v1/tasks` 的 body）。
// JSON 字段名与 TS 侧 `DispatchPayload` 逐字对应。
type DownloadTask struct {
	TaskID    string `json:"taskId"`
	TaskToken string `json:"taskToken"`
	Source    struct {
		URL string `json:"url"`
	} `json:"source"`
	Session struct {
		ID            string `json:"id"`
		Mode          string `json:"mode"` // single | multipart
		PartSize      int64  `json:"partSize"`
		PartCount     int    `json:"partCount"`
		Size          int64  `json:"size"`
		UploadedParts []int  `json:"uploadedParts"`
	} `json:"session"`
	Egress struct {
		Mode string `json:"mode"`
	} `json:"egress"`
	DeadlineMs int64 `json:"deadlineMs"`
}

// Runner 执行下载任务（一个任务一个 goroutine，槽位由 server 的信号量控制）。
type Runner struct {
	worker *WorkerClient
	log    *slog.Logger

	// 源站客户端：`warp`（WARP 出口）/ `direct`（直连或上游代理）时用它。
	source *http.Client

	// egressMode 取源站的方式（relay / warp / direct）。
	//
	// **由引擎配置决定，不看派发载荷**：Worker 侧的 `egress` 字段只是提示（当前恒为
	// relay），真正能不能直连/走 WARP 取决于这台机器的网络条件——那只有引擎自己知道。
	egressMode string

	// 进度回报的最小间隔（节流 D1 写入）。
	progressInterval time.Duration

	// 取消登记表（server 在受理/取消时增删；Runner 持有引用）。
	cancels *cancelRegistry
}

// NewRunner 构造执行器。
func NewRunner(worker *WorkerClient, source *http.Client, egressMode string, log *slog.Logger) *Runner {
	return &Runner{
		worker:           worker,
		source:           source,
		egressMode:       egressMode,
		log:              log,
		progressInterval: time.Second,
		cancels:          newCancelRegistry(),
	}
}

// cancelRegistry 取消登记表：taskID → cancel。server 在受理/取消时增删。
type cancelRegistry struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{cancels: map[string]context.CancelFunc{}}
}

func (r *cancelRegistry) add(taskID string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels[taskID] = cancel
}

func (r *cancelRegistry) remove(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, taskID)
}

func (r *cancelRegistry) cancel(taskID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cancel, ok := r.cancels[taskID]
	if ok {
		cancel()
		delete(r.cancels, taskID)
	}
	return ok
}

// lookup 查询任务是否在跑（重复派发判重）。
func (r *cancelRegistry) lookup(taskID string) (context.CancelFunc, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cancel, ok := r.cancels[taskID]
	return cancel, ok
}

// Run 执行一个任务到终态（成功/失败都由这里 finish；不向调用方返回错误——
// finish 的成败只影响 Worker 侧状态，引擎侧记录日志即可）。
func (r *Runner) Run(parentCtx context.Context, task DownloadTask) {
	ctx, cancel := context.WithCancel(parentCtx)
	r.cancels.add(task.TaskID, cancel)
	defer r.cancels.remove(task.TaskID)
	defer cancel()

	// 任务截止：Worker 载荷的 deadlineMs 是唯一时钟（72h 级）。
	if task.DeadlineMs > 0 {
		deadline := time.UnixMilli(task.DeadlineMs)
		if !deadline.After(time.Now()) {
			r.finish(ctx, task, FinishResult{OK: false, Code: "expired", Message: "任务已过期"})
			return
		}
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithDeadline(ctx, deadline)
		defer deadlineCancel()
	}

	log := r.log.With("task", task.TaskID)
	log.Info("开始下载", "url", task.Source.URL, "size", task.Session.Size, "mode", task.Session.Mode)

	if err := r.runParts(ctx, parentCtx, task, log); err != nil {
		code, message := classifyError(err)
		r.finish(ctx, task, FinishResult{OK: false, Code: code, Message: message})
		return
	}

	// 全部分片上传完成 → finish(ok)，Worker 侧自建清单核对并 complete。
	r.finish(ctx, task, FinishResult{OK: true})
	log.Info("下载完成", "size", task.Session.Size)
}

// runParts 逐片取字节并上传。
func (r *Runner) runParts(ctx context.Context, parentCtx context.Context, task DownloadTask, log *slog.Logger) error {
	// 已传分片集合（改派场景：跳过，不重传）。
	uploaded := make(map[int]bool, len(task.Session.UploadedParts))
	for _, n := range task.Session.UploadedParts {
		uploaded[n] = true
	}

	var downloaded int64
	for _, n := range task.Session.UploadedParts {
		downloaded += partLength(task.Session.Size, task.Session.PartSize, task.Session.PartCount, n)
	}

	if task.Session.Mode == "single" {
		// 单次会话：整个文件一个 PUT。已传过（重放）就直接 finish。
		if uploaded[1] {
			return nil
		}
		body, err := r.fetchRange(ctx, task, "bytes=0-")
		if err != nil {
			return err
		}
		defer body.Close()
		if err := r.worker.UploadSingle(ctx, task.TaskToken, task.Session.ID, task.Session.Size, body); err != nil {
			return err
		}
		_ = r.worker.ReportProgress(ctx, task.TaskToken, task.TaskID, task.Session.Size)
		return nil
	}

	lastProgress := time.Time{}
	for part := 1; part <= task.Session.PartCount; part++ {
		if ctx.Err() != nil {
			// 被取消（用户退订归零 / 引擎关闭）或超时：按引擎侧语义回报。
			if parentCtx.Err() == context.Canceled {
				return fmt.Errorf("任务被取消")
			}
			return fmt.Errorf("任务已超时")
		}
		if uploaded[part] {
			continue
		}

		start := int64(part-1) * task.Session.PartSize
		end := start + partLength(task.Session.Size, task.Session.PartSize, task.Session.PartCount, part) - 1
		rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end)
		length := end - start + 1

		// 单片取流 + 上传，带指数退避重试（≤5 次）。
		var lastErr error
		for attempt := 0; attempt < 5; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(1<<attempt) * time.Second):
				}
			}
			body, err := r.fetchRange(ctx, task, rangeHeader)
			if err != nil {
				lastErr = err
				log.Warn("取分片失败", "part", part, "attempt", attempt+1, "err", err)
				if isPermanent(err) {
					return err
				}
				continue
			}
			err = r.worker.UploadPart(ctx, task.TaskToken, task.Session.ID, part, length, body)
			_ = body.Close()
			if err == nil {
				downloaded += length
				lastErr = nil
				break
			}
			lastErr = err
			log.Warn("传分片失败", "part", part, "attempt", attempt+1, "err", err)
			if isPermanent(err) {
				return err
			}
		}
		if lastErr != nil {
			return lastErr
		}

		// 进度节流：每片或 ≥1s 一次。
		if time.Since(lastProgress) >= r.progressInterval {
			lastProgress = time.Now()
			if err := r.worker.ReportProgress(ctx, task.TaskToken, task.TaskID, downloaded); err != nil {
				log.Warn("进度回报失败（忽略）", "err", err)
			}
		}
	}
	// 最终进度补一次（确保 Worker 侧显示 100%）。
	_ = r.worker.ReportProgress(ctx, task.TaskToken, task.TaskID, downloaded)
	return nil
}

// fetchRange 取源站的一段字节。relay 走 Worker 的 fetch 端点；warp/direct 由引擎出网。
func (r *Runner) fetchRange(ctx context.Context, task DownloadTask, rangeHeader string) (io.ReadCloser, error) {
	if r.egressMode != "relay" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, task.Source.URL, nil)
		if err != nil {
			return nil, &PermanentError{Msg: "构造源站请求失败: " + err.Error()}
		}
		req.Header.Set("Range", rangeHeader)
		req.Header.Set("User-Agent", "Vultro-Engine/1.0")
		resp, err := r.source.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("源站返回 HTTP %d", resp.StatusCode)
		}
		return resp.Body, nil
	}

	// relay：经 Worker 出口（url 精确匹配白名单在 Worker 侧校验）。
	resp, err := r.worker.FetchRange(ctx, task.TaskToken, task.TaskID, task.Source.URL, rangeHeader)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		return nil, &PermanentError{Msg: "relay 出口拒绝（URL 与任务不匹配）"}
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("relay 取流 HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// finish 交还结果（失败只记录，不 panic）。
func (r *Runner) finish(_ context.Context, task DownloadTask, result FinishResult) {
	finishCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// 用 background 派生：ctx 可能已被取消（取消场景也要把 finish 送出去）。
	if err := r.worker.Finish(finishCtx, task.TaskToken, task.TaskID, result); err != nil {
		r.log.Error("finish 上报失败（Worker 侧 Cron 会兜底收敛）", "task", task.TaskID, "err", err)
	}
}

// partLength 第 n 片长度（除末片外等大；与 Worker 侧 expectedPartLength 同一公式）。
func partLength(size, partSize int64, partCount, partNumber int) int64 {
	if partNumber == partCount {
		return size - partSize*int64(partCount-1)
	}
	return partSize
}

// classifyError 引擎侧错误 → 结构化失败码（与 Worker 侧错误码表对齐）。
func classifyError(err error) (code, message string) {
	if pe, ok := err.(*PermanentError); ok {
		if containsAny(pe.Msg, "404", "410") {
			return "source-error-4xx", pe.Msg
		}
		return "source-error-5xx", pe.Msg
	}
	if errors.Is(err, context.DeadlineExceeded) || containsAny(err.Error(), "超时") {
		return "deadline", "任务超时"
	}
	if errors.Is(err, context.Canceled) || containsAny(err.Error(), "取消") {
		return "cancelled", "任务被取消"
	}
	return "source-error-5xx", err.Error()
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if contains(s, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// isPermanent 判断是否不可重试。
func isPermanent(err error) bool {
	_, ok := err.(*PermanentError)
	return ok
}
