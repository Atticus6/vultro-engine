// 出口构造的单测：relay 给零值客户端、direct 的代理协议校验。
//
// warp 分支需要真实 UDP 出网与 Cloudflare 注册，不做单测（失败语义由文档与启动日志保证）。
package egress

import (
	"io"
	"log/slog"
	"testing"

	"github.com/atticus6/vultro-engine/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewRelayReturnsClient(t *testing.T) {
	client, closeFn, err := New(&config.Config{EgressMode: "relay"}, discardLogger())
	if err != nil {
		t.Fatalf("relay 不应报错: %v", err)
	}
	if client == nil || closeFn == nil {
		t.Fatal("relay 应返回可用的客户端与 close 函数")
	}
}

func TestNewDirectRejectsUnsupportedProxy(t *testing.T) {
	_, _, err := New(&config.Config{
		EgressMode:  "direct",
		EgressProxy: "socks5://127.0.0.1:1080",
	}, discardLogger())
	if err == nil {
		t.Fatal("socks5 代理应报错（v1 只支持 http/https 上游）")
	}
}

func TestNewDirectWithHTTPProxy(t *testing.T) {
	client, _, err := New(&config.Config{
		EgressMode:  "direct",
		EgressProxy: "http://127.0.0.1:7890",
	}, discardLogger())
	if err != nil {
		t.Fatalf("http 代理应被接受: %v", err)
	}
	if client.Transport == nil {
		t.Fatal("direct + 代理时 Transport 应已配置")
	}
}
