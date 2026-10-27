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
	"testing"

	"github.com/atticus6/vultro-engine/internal/buildinfo"
	"github.com/atticus6/vultro-engine/internal/config"
	"github.com/atticus6/vultro-engine/internal/download"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{ID: "off1", Token: "tok", Capacity: 1}
	return New(cfg, download.NewRunner(nil, nil, "relay", log), log)
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
