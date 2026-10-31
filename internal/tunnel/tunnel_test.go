// 隧道辅助函数的单测：监听地址 → 隧道应访问的本机 URL。
package tunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func TestLocalURL(t *testing.T) {
	cases := map[string]string{
		":8787":          "http://127.0.0.1:8787", // 通配监听 → 回环
		"0.0.0.0:8787":   "http://127.0.0.1:8787",
		"[::]:8787":      "http://127.0.0.1:8787",
		"127.0.0.1:8787": "http://127.0.0.1:8787",
		"garbage":        "http://garbage",
	}
	for input, want := range cases {
		if got := LocalURL(input); got != want {
			t.Errorf("LocalURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestStopTunnelOnStop(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var exits []int
	exit := func(code int) { exits = append(exits, code) }

	// 隧道异常退出（带错误）→ 进程必须退出，交由守护进程重启。
	stopTunnelOnStop(context.Background(), errors.New("tunnel broken"), log, exit)
	// RunContext "正常"返回也意味着隧道已不可用：同样要退出。
	stopTunnelOnStop(context.Background(), nil, log, exit)
	// ctx 已取消 = 进程正在关闭：不退出。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	stopTunnelOnStop(cancelled, nil, log, exit)

	if len(exits) != 2 {
		t.Fatalf("应只在隧道异常停止时退出 2 次，实际 %v", exits)
	}
	for _, code := range exits {
		if code != 1 {
			t.Errorf("退出码应为 1，得到 %d", code)
		}
	}
}
