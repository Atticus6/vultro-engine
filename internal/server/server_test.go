// 入站 HTTP 服务的单测：鉴权、载荷校验、排水拒绝、健康检查与取消语义。
//
// 不覆盖"真正跑一个任务"的路径：Runner 会把任务交给 Worker，那属于 external 依赖，
// 由 internal/download 与集成环境负责。
package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atticus6/vultro-engine/internal/buildinfo"
	"github.com/atticus6/vultro-engine/internal/config"
	"github.com/atticus6/vultro-engine/internal/download"
	"github.com/atticus6/vultro-engine/internal/worker"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{ID: "off1", Token: "tok", Capacity: 1}
	return New(cfg, download.NewRunner(nil, nil, "relay", nil, log), log)
}

// do 发一个请求（请求体可选）并返回响应。
func do(t *testing.T, srv *Server, method, path, body string, authorized bool) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if authorized {
		req.Header.Set("Authorization", "Bearer tok")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

func TestDispatchRequiresToken(t *testing.T) {
	resp := do(t, newTestServer(t), http.MethodPost, "/v1/tasks", `{}`, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，得到 %d", resp.StatusCode)
	}
}

func TestDispatchRejectsBadPayload(t *testing.T) {
	srv := newTestServer(t)
	for name, body := range map[string]string{
		"非法 JSON": "{",
		"缺字段":     `{}`,
	} {
		resp := do(t, srv, http.MethodPost, "/v1/tasks", body, true)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: 应 400，得到 %d", name, resp.StatusCode)
		}
	}
}

func TestDispatchRejectsWhileDraining(t *testing.T) {
	srv := newTestServer(t)
	srv.draining.Store(true)

	payload := `{"taskId":"t1","taskToken":"tok","source":{"url":"https://example.com/a"}}`
	resp := do(t, srv, http.MethodPost, "/v1/tasks", payload, true)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("排水中应 503，得到 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After 应为 30，得到 %q", got)
	}
}

func TestHealth(t *testing.T) {
	resp := do(t, newTestServer(t), http.MethodGet, "/v1/healthz", "", false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz 应 200，得到 %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body["version"] != buildinfo.Version {
		t.Errorf("version = %v, want %q", body["version"], buildinfo.Version)
	}
	if body["id"] != "off1" {
		t.Errorf("id = %v, want off1", body["id"])
	}
	if body["draining"] != false {
		t.Errorf("draining = %v, want false", body["draining"])
	}
}

func TestCancelUnknownTask(t *testing.T) {
	resp := do(t, newTestServer(t), http.MethodPost, "/v1/tasks/nope/cancel", "", true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("取消未知任务应 202（幂等），得到 %d", resp.StatusCode)
	}
	var body map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body["cancelled"] {
		t.Error("未知任务不应报告 cancelled=true")
	}
}

// TestQueuedTaskIsRegisteredAndCancelable 排队中的任务必须"受理即登记"：
// 只有这样才能取消（否则返回 cancelled=false、槽位空出后照常下载），
// 重放也才能判重（否则同一 taskId 被再次入队、同一批分片被重复上传）。
func TestQueuedTaskIsRegisteredAndCancelable(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	allowRun := func() { releaseOnce.Do(func() { close(release) }) }
	defer allowRun() // 测试提前失败时也放行，避免下面 Close 卡在阻塞的 handler 上

	fetchStarted := make(chan struct{}, 1)
	finishBodies := make(chan string, 8)
	workerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/fetch"):
			select {
			case fetchStarted <- struct{}{}:
			default:
			}
			<-release // 占住引擎的唯一槽位，直到测试放行
		case strings.HasSuffix(r.URL.Path, "/finish"):
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<10))
			finishBodies <- string(body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer workerSrv.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := worker.NewClient(workerSrv.URL, "tok", workerSrv.Client(), log)
	runner := download.NewRunner(client, nil, "relay", nil, log)
	srv := New(&config.Config{ID: "off1", Token: "tok", Capacity: 1}, runner, log)

	payload := func(id string) string {
		return `{"taskId":"` + id + `","taskToken":"tt",` +
			`"source":{"url":"https://example.com/a"},` +
			`"session":{"id":"s-1","mode":"single","size":5}}`
	}

	// A 占住唯一的槽位（卡在 relay 取流）。
	if resp := do(t, srv, http.MethodPost, "/v1/tasks", payload("a"), true); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("A 应 202，得到 %d", resp.StatusCode)
	}
	<-fetchStarted

	// 重放 A：幂等 202 + duplicate，不再入队。
	dupResp := do(t, srv, http.MethodPost, "/v1/tasks", payload("a"), true)
	var dup map[string]bool
	if err := json.NewDecoder(dupResp.Body).Decode(&dup); err != nil {
		t.Fatalf("解析重放响应失败: %v", err)
	}
	if !dup["duplicate"] {
		t.Fatalf("重放应返回 duplicate=true，得到 %v", dup)
	}

	// B 在排队（槽位被 A 占着）：受理即登记，此刻就必须能判重/取消。
	if resp := do(t, srv, http.MethodPost, "/v1/tasks", payload("b"), true); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("B 应 202（排队），得到 %d", resp.StatusCode)
	}
	if !runner.IsRunning("b") {
		t.Fatal("排队中的任务必须已在登记表里（否则取消落空、重放会重复入队）")
	}

	cancelResp := do(t, srv, http.MethodPost, "/v1/tasks/b/cancel", "", true)
	var cancelled map[string]bool
	if err := json.NewDecoder(cancelResp.Body).Decode(&cancelled); err != nil {
		t.Fatalf("解析取消响应失败: %v", err)
	}
	if !cancelled["cancelled"] {
		t.Fatalf("排队任务取消应生效，得到 %v", cancelled)
	}

	// 取消后必须尽快 finish(cancelled)，而不是等槽位空出来才开始下载。
	select {
	case body := <-finishBodies:
		if !strings.Contains(body, "cancelled") {
			t.Errorf("B 的 finish 应回报 cancelled，得到 %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("被取消的排队任务没有 finish（goroutine 仍在等槽位）")
	}

	allowRun()
}
