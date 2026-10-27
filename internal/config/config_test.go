// 配置解析的单测：默认值、必填项、枚举与数值校验。
package config

import "testing"

func TestLoad(t *testing.T) {
	t.Setenv("ENGINE_ID", "off1")
	t.Setenv("ENGINE_PUBLIC_URL", "")
	t.Setenv("ENGINE_TOKEN", "tok")
	t.Setenv("WORKER_BASE_URL", "http://localhost:8787/")
	t.Setenv("EGRESS_MODE", "")
	t.Setenv("ENGINE_CAPACITY", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("完整配置不应报错: %v", err)
	}
	if cfg.WorkerBaseURL != "http://localhost:8787" {
		t.Errorf("基址尾部斜杠应被裁掉，得到 %q", cfg.WorkerBaseURL)
	}
	// ENGINE_PUBLIC_URL 留空 = 使用内嵌 quick tunnel（不再必填）。
	if !cfg.UsesQuickTunnel() {
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
	if _, err := Load(); err == nil {
		t.Fatal("缺 ENGINE_TOKEN 应报错")
	}

	// 非法出口模式。
	t.Setenv("ENGINE_TOKEN", "tok")
	t.Setenv("EGRESS_MODE", "weird")
	if _, err := Load(); err == nil {
		t.Fatal("非法 EGRESS_MODE 应报错")
	}
}

func TestCapacityValidation(t *testing.T) {
	t.Setenv("ENGINE_ID", "off1")
	t.Setenv("ENGINE_PUBLIC_URL", "https://off1.example.com")
	t.Setenv("ENGINE_TOKEN", "tok")
	t.Setenv("WORKER_BASE_URL", "http://localhost:8787")
	t.Setenv("EGRESS_MODE", "relay")
	t.Setenv("ENGINE_CAPACITY", "17")
	if _, err := Load(); err == nil {
		t.Fatal("容量超上界应报错")
	}
}

func TestTunnelAndEgressModes(t *testing.T) {
	t.Setenv("ENGINE_ID", "off1")
	t.Setenv("ENGINE_TOKEN", "tok")
	t.Setenv("WORKER_BASE_URL", "http://localhost:8787")
	t.Setenv("ENGINE_PUBLIC_URL", "")
	t.Setenv("TUNNEL_PROTOCOL", "quic")
	t.Setenv("EGRESS_MODE", "warp")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("warp + quic 应被接受: %v", err)
	}
	if cfg.EgressMode != "warp" || cfg.TunnelProtocol != "quic" {
		t.Fatalf("配置未生效: %+v", cfg)
	}

	t.Setenv("TUNNEL_PROTOCOL", "udp")
	if _, err := Load(); err == nil {
		t.Fatal("非法 TUNNEL_PROTOCOL 应报错")
	}

	t.Setenv("TUNNEL_PROTOCOL", "http2")
	t.Setenv("EGRESS_MODE", "tor")
	if _, err := Load(); err == nil {
		t.Fatal("非法 EGRESS_MODE 应报错")
	}
}
