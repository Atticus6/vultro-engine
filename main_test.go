// 引擎核心逻辑的单测：分片公式（与 Worker 侧逐字一致）、错误分类、取消登记表、配置。
package main

import (
	"context"
	"testing"
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
		{&PermanentError{Msg: "HTTP 404: nope"}, "source-error-4xx"},
		{&PermanentError{Msg: "HTTP 410: gone"}, "source-error-4xx"},
		{&PermanentError{Msg: "HTTP 502: bad gateway"}, "source-error-5xx"},
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

func TestLoadConfig(t *testing.T) {
	t.Setenv("ENGINE_ID", "off1")
	t.Setenv("ENGINE_PUBLIC_URL", "")
	t.Setenv("ENGINE_TOKEN", "tok")
	t.Setenv("WORKER_BASE_URL", "http://localhost:8787/")
	t.Setenv("EGRESS_MODE", "")
	t.Setenv("ENGINE_CAPACITY", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("完整配置不应报错: %v", err)
	}
	if cfg.WorkerBaseURL != "http://localhost:8787" {
		t.Errorf("基址尾部斜杠应被裁掉，得到 %q", cfg.WorkerBaseURL)
	}
	// ENGINE_PUBLIC_URL 留空 = 使用内嵌 quick tunnel（不再必填）。
	if !cfg.usesQuickTunnel() {
		t.Error("ENGINE_PUBLIC_URL 为空时应启用内嵌隧道")
	}
	if cfg.TunnelProtocol != "http2" {
		t.Errorf("默认隧道协议应为 http2（UDP 常被封），得到 %q", cfg.TunnelProtocol)
	}
	if cfg.EgressMode != "relay" {
		t.Errorf("默认出口应为 relay，得到 %q", cfg.EgressMode)
	}
	if cfg.Capacity != 2 {
		t.Errorf("默认槽位应为 2，得到 %d", cfg.Capacity)
	}

	// 缺必需项 → 启动失败（Fail Fast）。
	t.Setenv("ENGINE_TOKEN", "")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("缺 ENGINE_TOKEN 应报错")
	}

	// 非法出口模式。
	t.Setenv("ENGINE_TOKEN", "tok")
	t.Setenv("EGRESS_MODE", "weird")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("非法 EGRESS_MODE 应报错")
	}
}

func TestConfigCapacityValidation(t *testing.T) {
	t.Setenv("ENGINE_ID", "off1")
	t.Setenv("ENGINE_PUBLIC_URL", "https://off1.example.com")
	t.Setenv("ENGINE_TOKEN", "tok")
	t.Setenv("WORKER_BASE_URL", "http://localhost:8787")
	t.Setenv("EGRESS_MODE", "relay")
	t.Setenv("ENGINE_CAPACITY", "17")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("容量超上界应报错")
	}
}

func TestConfigTunnelAndEgressModes(t *testing.T) {
	t.Setenv("ENGINE_ID", "off1")
	t.Setenv("ENGINE_TOKEN", "tok")
	t.Setenv("WORKER_BASE_URL", "http://localhost:8787")
	t.Setenv("ENGINE_PUBLIC_URL", "")
	t.Setenv("TUNNEL_PROTOCOL", "quic")
	t.Setenv("EGRESS_MODE", "warp")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("warp + quic 应被接受: %v", err)
	}
	if cfg.EgressMode != "warp" || cfg.TunnelProtocol != "quic" {
		t.Fatalf("配置未生效: %+v", cfg)
	}

	t.Setenv("TUNNEL_PROTOCOL", "udp")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("非法 TUNNEL_PROTOCOL 应报错")
	}

	t.Setenv("TUNNEL_PROTOCOL", "http2")
	t.Setenv("EGRESS_MODE", "tor")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("非法 EGRESS_MODE 应报错")
	}
}

func TestNormalizeAddr(t *testing.T) {
	cases := map[string]string{
		":8787":          ":8787",
		"0.0.0.0:8787":   ":8787",
		"127.0.0.1:8787": "127.0.0.1:8787",
		"garbage":        "garbage",
	}
	for input, want := range cases {
		if got := normalizeAddr(input); got != want {
			t.Errorf("normalizeAddr(%q) = %q, want %q", input, got, want)
		}
	}
}
