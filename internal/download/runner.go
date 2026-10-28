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

// IsRunning 任务是否在跑（重复派发判重用）。
func (r *Runner) IsRunning(taskID string) bool {
	_, running := r.cancels.lookup(taskID)
	return running
}

// cancelRegistry 取消登记表：taskID → cancel。
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
func (r *Runner) Run(parentCtx context.Context, task Task) {
	ctx, cancel := context.WithCancel(parentCtx)
	r.cancels.add(task.TaskID, cancel)
	defer r.cancels.remove(task.TaskID)
	defer cancel()

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

	if err := r.runParts(ctx, parentCtx, task, log); err != nil {
		code, message := classifyError(err)
		r.finish(task, worker.FinishResult{OK: false, Code: code, Message: message})
		return
	}

	// 全部分片上传完成 → finish(ok)，Worker 侧自建清单核对并 complete。
	r.finish(task, worker.FinishResult{OK: true})
	log.Info("下载完成", "size", task.Session.Size)
}

// runParts 逐片取字节并上传。
func (r *Runner) runParts(ctx context.Context, parentCtx context.Context, task Task, log *slog.Logger) error {
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
			return nil, fmt.Errorf("源站返回 HTTP %d", resp.StatusCode)
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
		return nil, &worker.PermanentError{Msg: "relay 出口拒绝（URL 与任务不匹配）"}
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("relay 取流 HTTP %d", resp.StatusCode)
	}
	return r.rxReader(resp.Body), nil
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
