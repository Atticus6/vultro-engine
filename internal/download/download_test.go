// 下载核心逻辑的单测：分片公式（与 Worker 侧逐字一致）、错误分类、取消登记表。
package download

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/atticus6/vultro-engine/internal/worker"
)

func TestPartLength(t *testing.T) {
	// 100 字节 ÷ 32 字节片 = 3 片：32/32/36（末片余数）。
	size, partSize, count := int64(100), int64(32), 3
	cases := []struct {
		part int
		want int64
	}{
		{1, 32},
		{2, 32},
		{3, 36}, // 末片 = 100 - 32*2
	}
	for _, c := range cases {
		if got := partLength(size, partSize, count, c.part); got != c.want {
			t.Errorf("partLength(%d) = %d, want %d", c.part, got, c.want)
		}
	}
	// 恰好整除：末片 = partSize。
	if got := partLength(64, 32, 2, 2); got != 32 {
		t.Errorf("整除末片 = %d, want 32", got)
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		err      error
		wantCode string
	}{
		{&worker.PermanentError{Msg: "HTTP 404: nope"}, "source-error-4xx"},
		{&worker.PermanentError{Msg: "HTTP 410: gone"}, "source-error-4xx"},
		{&worker.PermanentError{Msg: "HTTP 502: bad gateway"}, "source-error-5xx"},
		// 带 Status 的按状态码归类：403 这类不在消息里出现 404/410 的 4xx 也要判对。
		{&worker.PermanentError{Msg: "源站返回 HTTP 403", Status: 403}, "source-error-4xx"},
		{&worker.PermanentError{Msg: "HTTP 503: busy", Status: 503}, "source-error-5xx"},
		{context.DeadlineExceeded, "deadline"},
		{context.Canceled, "cancelled"},
	}
	for _, c := range cases {
		code, _ := classifyError(c.err)
		if code != c.wantCode {
			t.Errorf("classifyError(%v) = %q, want %q", c.err, code, c.wantCode)
		}
	}
}

func TestCancelRegistry(t *testing.T) {
	registry := newCancelRegistry()

	ctx, done, already := registry.begin(context.Background(), "task-1")
	if already {
		t.Fatal("首次登记不应判成重复")
	}
	if _, ok := registry.lookup("task-1"); !ok {
		t.Fatal("begin 后 lookup 应命中")
	}

	// 同 id 再登记：锁内判重，返回 already 而不是覆盖前一个 cancel。
	if _, _, dup := registry.begin(context.Background(), "task-1"); !dup {
		t.Fatal("重复登记应返回 already=true")
	}

	if !registry.cancel("task-1") {
		t.Fatal("cancel 应命中已登记任务（含还在排队、尚未开始跑的任务）")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancel 后 ctx 应已取消")
	}
	if registry.cancel("task-1") {
		t.Fatal("重复 cancel 应返回 false（已删除）")
	}
	if _, ok := registry.lookup("task-1"); ok {
		t.Fatal("cancel 后 lookup 不应命中")
	}

	// done 必须幂等（Run 的 defer 与调用方兜底都可能调用）。
	done()
	done()
}

// TestRunPartsReportsCancelNotDeadline 取消必须回报 cancelled，不能报成 deadline。
//
// 回归点：原判据看的是 `parentCtx.Err()`，而调度方传的是 `context.Background()`——
// 它永远不会被取消，于是所有用户取消都被回报成「任务已超时」。
func TestRunPartsReportsCancelNotDeadline(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := NewRunner(nil, nil, "relay", nil, log)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟用户退订：任务 ctx 已被取消（还没开始下一片）

	var task Task
	task.TaskID = "t1"
	task.Session.Mode = "multipart"
	task.Session.PartCount = 1
	task.Session.PartSize = 10
	task.Session.Size = 10

	err := runner.runParts(ctx, task, log)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runParts 应返回 context.Canceled，得到 %v", err)
	}
	if code, _ := classifyError(err); code != "cancelled" {
		t.Errorf("classifyError = %q, want cancelled", code)
	}
}

func TestSourceStatusError(t *testing.T) {
	cases := []struct {
		status    int
		permanent bool
	}{
		{403, true},  // 无权限：重试无意义
		{404, true},  // 链接失效
		{408, false}, // 请求超时：稍后重试
		{429, false}, // 限流：稍后重试
		{500, false}, // 源站抖动：可重试
	}
	for _, c := range cases {
		err := sourceStatusError(c.status)
		var permanent *worker.PermanentError
		if got := errors.As(err, &permanent); got != c.permanent {
			t.Errorf("sourceStatusError(%d) 的 PermanentError 判定 = %v, want %v（err=%v）",
				c.status, got, c.permanent, err)
		}
		if c.permanent {
			if code, _ := classifyError(err); code != "source-error-4xx" {
				t.Errorf("classifyError(sourceStatusError(%d)) = %q, want source-error-4xx", c.status, code)
			}
		}
	}
}
