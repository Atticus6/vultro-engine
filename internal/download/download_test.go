// 下载核心逻辑的单测：分片公式（与 Worker 侧逐字一致）、错误分类、取消登记表。
package download

import (
	"context"
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
	ctx, cancel := context.WithCancel(context.Background())

	registry.add("task-1", cancel)
	if _, ok := registry.lookup("task-1"); !ok {
		t.Fatal("add 后 lookup 应命中")
	}
	if !registry.cancel("task-1") {
		t.Fatal("cancel 应命中已登记任务")
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
}
