// Worker 客户端：引擎 → Worker 的全部出站调用（注册/心跳/进度/finish/relay fetch/分片上传）。
//
// 所有请求都带引擎令牌或任务令牌（Bearer）；对 5xx/网络错误做指数退避重试，
// 4xx（除 408/429）视为终态失败不重试——那通常是状态已变（会话被取消/过期），
// 重试只会撞同一堵墙。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// WorkerClient 引擎对 Worker 的出站客户端。
type WorkerClient struct {
	baseURL string
	token   string
	http    *http.Client
	log     *slog.Logger
}

// NewWorkerClient 构造客户端。http.Client 由调用方注入（direct 模式时带代理 Transport）。
func NewWorkerClient(baseURL, token string, httpClient *http.Client, log *slog.Logger) *WorkerClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Minute}
	}
	return &WorkerClient{baseURL: baseURL, token: token, http: httpClient, log: log}
}

// do 发一个 JSON 请求并解析 JSON 响应；带退避重试。
func (w *WorkerClient) do(ctx context.Context, method, path string, body any, out any, bearer string) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码请求体: %w", err)
		}
	}

	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 指数退避：1s → 2s → 4s。等待可被 ctx 取消。
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<(attempt-1)) * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, w.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("构造请求: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)

		resp, err := w.http.Do(req)
		if err != nil {
			lastErr = err
			w.log.Warn("worker 请求失败，将重试", "path", path, "attempt", attempt+1, "err", err)
			continue
		}

		// 4xx（除 408/429）= 状态已变，重试无意义；5xx/429 = 可重试。
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out != nil {
				if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil && err != io.EOF {
					resp.Body.Close()
					return fmt.Errorf("解析响应: %w", err)
				}
			}
			resp.Body.Close()
			return nil
		}
		if (resp.StatusCode >= 400 && resp.StatusCode < 500) && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			resp.Body.Close()
			return &PermanentError{Msg: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, snippet)}
		}
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		resp.Body.Close()
		lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
		w.log.Warn("worker 请求可重试失败", "path", path, "attempt", attempt+1, "status", resp.StatusCode)
	}
	return fmt.Errorf("重试耗尽: %w", lastErr)
}

// PermanentError 不可重试的错误（4xx 语义）。
type PermanentError struct{ Msg string }

func (e *PermanentError) Error() string { return e.Msg }

// Register 引擎注册（幂等更新自己那一行）。启动时一次。
//
// **不发送 id**：身份完全由令牌决定——Worker 按 `Authorization` 里的 `eng_…` 反查节点行。
// 请求体里再带一个 id 会让"我是谁"出现两个来源（而其中一个可以随便填）。引擎 id 仍留在
// 本地配置里，因为日志与运维对账要用它显示。
func (w *WorkerClient) Register(ctx context.Context, cfg *Config) error {
	return w.do(ctx, http.MethodPost, "/api/offline/engines/register", map[string]any{
		"publicUrl": cfg.PublicURL,
		"capacity":  cfg.Capacity,
		"version":   Version,
	}, nil, w.token)
}

// Heartbeat 心跳：刷新 lastSeenAt、上报版本与排水状态；返回 dispatchHint。
//
// **不发送 x-engine-id**：节点身份同样只来自令牌。旧实现用这个头告诉 Worker "我是哪台"，
// 那意味着持有令牌的人能替别的节点报活——共享令牌时代尤其致命。
//
// 上报 `draining` 是关键：Worker 据此把本节点从派发候选里排除——否则排空期间
// 新任务还会被派过来（引擎虽然会 503，但那要多一次往返且任务要重新排队）。
func (w *WorkerClient) Heartbeat(ctx context.Context, engineID, version string, draining bool) (bool, error) {
	var out struct {
		DispatchHint bool `json:"dispatchHint"`
	}
	body, err := json.Marshal(map[string]any{
		"version":  version,
		"draining": draining,
	})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		w.baseURL+"/api/offline/engines/heartbeat", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.token)
	resp, err := w.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("heartbeat HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	// engineID 只用于调用方日志；身份已经由令牌表达，不再上网。
	_ = engineID
	return out.DispatchHint, nil
}

// ReportProgress 进度回报（节流由调用方负责）。
func (w *WorkerClient) ReportProgress(ctx context.Context, taskToken string, taskID string, downloaded int64) error {
	return w.do(ctx, http.MethodPost, "/api/offline/tasks/"+taskID+"/progress",
		map[string]any{"downloadedBytes": downloaded}, nil, taskToken)
}

// FinishResult finish 载荷：成功或结构化失败。
type FinishResult struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Finish 交还结果（成功 → Worker 结算扇出；失败 → abort + 传导）。
func (w *WorkerClient) Finish(ctx context.Context, taskToken string, taskID string, result FinishResult) error {
	return w.do(ctx, http.MethodPost, "/api/offline/tasks/"+taskID+"/finish", result, nil, taskToken)
}

// FetchRange relay 出口：经 Worker 取源站的一段字节（源站只见 Cloudflare 出口）。
// 返回上游响应（调用方负责关闭 body）。
func (w *WorkerClient) FetchRange(ctx context.Context, taskToken string, taskID, url, rangeHeader string) (*http.Response, error) {
	body, err := json.Marshal(map[string]any{"url": url, "range": rangeHeader})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		w.baseURL+"/api/offline/tasks/"+taskID+"/fetch", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+taskToken)
	return w.http.Do(req)
}

// UploadPart 分片上传：直 PUT Worker 的既有分片路由（与浏览器客户端同一契约）。
// body 长度必须精确等于 expected（服务端按字节长度硬校验）。
func (w *WorkerClient) UploadPart(ctx context.Context, taskToken string, sessionID string, partNumber int, expected int64, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/api/upload/%s/part/%d", w.baseURL, sessionID, partNumber), body)
	if err != nil {
		return err
	}
	req.ContentLength = expected
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+taskToken)

	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
		return &PermanentError{Msg: fmt.Sprintf("分片上传 HTTP %d: %s", resp.StatusCode, snippet)}
	}
	return fmt.Errorf("分片上传 HTTP %d: %s", resp.StatusCode, snippet)
}

// UploadSingle 单次上传（≤8 MiB 的会话走这条）。
func (w *WorkerClient) UploadSingle(ctx context.Context, taskToken string, sessionID string, expected int64, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/api/upload/%s/content", w.baseURL, sessionID), body)
	if err != nil {
		return err
	}
	req.ContentLength = expected
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+taskToken)

	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return fmt.Errorf("单次上传 HTTP %d: %s", resp.StatusCode, snippet)
}
