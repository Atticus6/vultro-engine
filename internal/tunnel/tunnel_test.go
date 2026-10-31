// 隧道辅助函数的单测：监听地址 → 隧道应访问的本机 URL。
package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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

func TestStdioCaptureBufferIsBounded(t *testing.T) {
	capture := &stdioCapture{}
	chunk := bytes.Repeat([]byte("x"), 8<<10)

	// 写 20 块（160 KiB）远超上限。
	for i := 0; i < 20; i++ {
		chunk[0] = byte('a' + i) // 每块首字节不同，便于验证留下的是"最近"内容
		capture.appendOutput(chunk)
	}

	out := capture.String()
	if len(out) > maxCapturedBytes {
		t.Fatalf("缓冲应被裁到 %d 字节以内，实际 %d", maxCapturedBytes, len(out))
	}
	// 最后写入的那一块必须完整保留（启动阶段的地址/就绪行都在早期日志里，
	// 裁剪只丢最老的部分）。
	if !strings.HasSuffix(out, string(chunk)) {
		t.Error("缓冲应保留最近写入的完整一块")
	}
}
