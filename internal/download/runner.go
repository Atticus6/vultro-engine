// 下载循环：逐片 Range GET → 内存一片 → 分片上传 Worker → finish 交还结果。
//
// 一个任务一个 goroutine；并发上限（槽位）由 server 的信号量控制，不在这里做。
package download

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/atticus6/vultro-engine/internal/stats"
	"github.com/atticus6/vultro-engine/internal/worker"
)

// Runner 执行下载任务。
type Runner struct {
	client *worker.Client
	log    *slog.Logger

	// 源站客户端：`warp`（WARP 出口）/ `direct`（直连或上游代理）时用它。
	source *http.Client

	// egressMode 取源站的方式（relay / warp / direct）。
	//
	// **由引擎配置决定，不看派发载荷**：Worker 侧的 `egress` 字段只是提示（当前恒为
	// relay），真正能不能直连/走 WARP 取决于这台机器的网络条件——那只有引擎自己知道。
	egressMode string

	// counters 进程级流量计数（心跳上报给 Worker）。nil = 不统计（单测构造时留空）。
	counters *stats.Counters

	// 进度回报的最小间隔（节流 D1 写入）。
	progressInterval time.Duration

	// 取消登记表：受理/取消/判重都经由 Runner 的方法访问，server 不直接碰它。
	cancels *cancelRegistry
}

// NewRunner 构造执行器。
//
// counters 为 nil 时不统计流量（单测直接构造 Runner 的场景）；生产恒有值，见 main.go。
func NewRunner(
	client *worker.Client,
	source *http.Client,
	egressMode string,
	counters *stats.Counters,
	log *slog.Logger,
) *Runner {
	return &Runner{
		client:           client,
		source:           source,
		egressMode:       egressMode,
		counters:         counters,
		log:              log,
		progressInterval: time.Second,
		cancels:          newCancelRegistry(),
	}
}

// Cancel 置取消标记：当前分片做完即停（≤1 片的浪费）。返回任务是否存在。
func (r *Runner) Cancel(taskID string) bool {
	return r.cancels.cancel(taskID)
}

// IsRunning 任务是否在排队或运行中（登记表里有就是）。
func (r *Runner) IsRunning(taskID string) bool {
	_, running := r.cancels.lookup(taskID)
	return running
}

// Begin 在**受理时**登记任务（判重 + 可取消 ctx），必须在等槽位之前调用。
//
// 为什么不能等 Run 里再登记：排队期间任务不在登记表里，Cancel 会落空（返回
// cancelled=false，槽位空出后照常下载），IsRunning 也判不出重放（同一 taskId 被再次
// 入队，同一批分片被重复下载/上传）。
//
// done 必须在任务结束时调用（注销登记）；already 为真表示同 id 正在排队/运行，调用方
// 应直接按幂等 202 返回，不要重复入队。
func (r *Runner) Begin(parent context.Context, taskID string) (context.Context, func(), bool) {
	return r.cancels.begin(parent, taskID)
}

// cancelRegistry 取消登记表：taskID → cancel。
type cancelRegistry struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{cancels: map[string]context.CancelFunc{}}
}

// begin 在锁内完成「判重 + 登记」：两次并发派发只会有一个成功，另一个拿到 already。
func (r *cancelRegistry) begin(parent context.Context, taskID string) (context.Context, func(), bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.cancels[taskID]; ok {
		return nil, nil, true
	}
	ctx, cancel := context.WithCancel(parent)
	r.cancels[taskID] = cancel

	// 注销必须幂等：Run 的 defer 与调用方的兜底都可能触发它。
	var once sync.Once
	done := func() {
		once.Do(func() {
			cancel()
			r.mu.Lock()
			defer r.mu.Unlock()
			delete(r.cancels, taskID)
		})
	}
	return ctx, done, false
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
func (r *Runner) Run(ctx context.Context, task Task) {
	/*
	 * 排队期间就被取消：不下载、不占槽位，直接按取消回报。ctx 由 `Begin` 在受理时
	 * 创建并登记，所以「还没轮到就跑不了」与「运行中途被取消」在这里是同一个判断。
	 */
	if ctx.Err() != nil {
		r.finish(task, worker.FinishResult{OK: false, Code: "cancelled", Message: "任务被取消"})
		return
	}

	// 任务截止：Worker 载荷的 deadlineMs 是唯一时钟（72h 级）。
	if task.DeadlineMs > 0 {
		deadline := time.UnixMilli(task.DeadlineMs)
		if !deadline.After(time.Now()) {
			r.finish(task, worker.FinishResult{OK: false, Code: "expired", Message: "任务已过期"})
			return
		}
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithDeadline(ctx, deadline)
		defer deadlineCancel()
	}

	log := r.log.With("task", task.TaskID)
	log.Info("开始下载", "url", task.Source.URL, "size", task.Session.Size, "mode", task.Session.Mode)

	if err := r.runParts(ctx, task, log); err != nil {
		code, message := classifyError(err)
		r.finish(task, worker.FinishResult{OK: false, Code: code, Message: message})
		return
	}

	// 全部分片上传完成 → finish(ok)，Worker 侧自建清单核对并 complete。
	r.finish(task, worker.FinishResult{OK: true})
	log.Info("下载完成", "size", task.Session.Size)
}

// runParts 逐片取字节并上传。
func (r *Runner) runParts(ctx context.Context, task Task, log *slog.Logger) error {
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
		if err := r.client.UploadSingle(
			ctx, task.TaskToken, task.Session.ID, task.Session.Size, r.txReader(body),
		); err != nil {
			return err
		}
		_ = r.client.ReportProgress(ctx, task.TaskToken, task.TaskID, task.Session.Size)
		return nil
	}

	lastProgress := time.Time{}
	for part := 1; part <= task.Session.PartCount; part++ {
		/*
		 * 直接返回 ctx 的错误**类型**：`classifyError` 用 `errors.Is` 区分
		 * `context.Canceled`（用户退订归零 / 引擎关闭 → cancelled）与
		 * `context.DeadlineExceeded`（任务截止 → deadline）。
		 *
		 * 不能用父 ctx 判断：调度方传的是 `context.Background()`，它的 `Err()` 恒为
		 * nil——用户取消会全部被报成「任务已超时」，Worker 收到错误的终态原因。
		 */
		if err := ctx.Err(); err != nil {
			return err
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
			err = r.client.UploadPart(
				ctx, task.TaskToken, task.Session.ID, part, length, r.txReader(body),
			)
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
			if err := r.client.ReportProgress(ctx, task.TaskToken, task.TaskID, downloaded); err != nil {
				log.Warn("进度回报失败（忽略）", "err", err)
			}
		}
	}
	// 最终进度补一次（确保 Worker 侧显示 100%）。
	_ = r.client.ReportProgress(ctx, task.TaskToken, task.TaskID, downloaded)
	return nil
}

// fetchRange 取源站的一段字节。relay 走 Worker 的 fetch 端点；warp/direct 由引擎出网。
//
// 回来的 body 已经套上**下行计数**：按实际读出的字节记账（不是分片长度）——失败重试的
// 那一片同样占了带宽，按声明长度算会少记。
func (r *Runner) fetchRange(ctx context.Context, task Task, rangeHeader string) (io.ReadCloser, error) {
	if r.egressMode != "relay" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, task.Source.URL, nil)
		if err != nil {
			return nil, &worker.PermanentError{Msg: "构造源站请求失败: " + err.Error()}
		}
		req.Header.Set("Range", rangeHeader)
		req.Header.Set("User-Agent", "Vultro-Engine/1.0")
		resp, err := r.source.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, sourceStatusError(resp.StatusCode)
		}
		return r.rxReader(resp.Body), nil
	}

	// relay：经 Worker 出口（url 精确匹配白名单在 Worker 侧校验）。
	resp, err := r.client.FetchRange(ctx, task.TaskToken, task.TaskID, task.Source.URL, rangeHeader)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		return nil, &worker.PermanentError{Msg: "relay 出口拒绝（URL 与任务不匹配）", Status: resp.StatusCode}
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, sourceStatusError(resp.StatusCode)
	}
	return r.rxReader(resp.Body), nil
}

// sourceStatusError 把源站 / relay 的非成功状态翻译成错误：4xx（408/429 除外）是
// **不可重试**的（链接失效、无权限），其余按可重试的临时错误处理。
//
// 必须返回 *worker.PermanentError：`isPermanent` 只认这个类型。此前一律 `fmt.Errorf`，
// 一个失效链接要重试 5 次（约 30 秒退避），最终还被 `classifyError` 归到
// source-error-5xx。408/429 显式保留为可重试：它们就是"稍后再来"的语义。
func sourceStatusError(status int) error {
	msg := fmt.Sprintf("源站返回 HTTP %d", status)
	if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
		return &worker.PermanentError{Msg: msg, Status: status}
	}
	return fmt.Errorf("%s", msg)
}

// rxReader 给源站响应体套下行计数；没有计数器时原样返回（单测构造）。
func (r *Runner) rxReader(body io.ReadCloser) io.ReadCloser {
	if r.counters == nil {
		return body
	}
	return r.counters.RxReader(body)
}

// txReader 给上传请求体套上行计数；没有计数器时原样返回。
//
// 计的是 HTTP 客户端**实际读出**的字节：中途失败的那次上传也被读了一部分，那同样是
// 真实流量，按"成功分片数 × 片长"统计会漏掉它们。
func (r *Runner) txReader(body io.Reader) io.Reader {
	if r.counters == nil {
		return body
	}
	return r.counters.TxReader(body)
}

// finish 交还结果（失败只记录，不 panic）。
func (r *Runner) finish(task Task, result worker.FinishResult) {
	// 用 background 派生：任务 ctx 可能已被取消（取消场景也要把 finish 送出去）。
	finishCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.client.Finish(finishCtx, task.TaskToken, task.TaskID, result); err != nil {
		r.log.Error("finish 上报失败（Worker 侧 Cron 会兜底收敛）", "task", task.TaskID, "err", err)
	}
}
