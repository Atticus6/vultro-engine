// Package egress 构造"取源站"的 HTTP 出口。
//
// 三种模式（由 EGRESS_MODE 选择，见 internal/config）：
//
//   - relay（默认）：经 Worker 转发，源站只见 Cloudflare IP——这个包给一个零值客户端，
//     实际取流走 internal/download 的 Worker fetch 分支；
//   - warp：经 WARP（用户态 WireGuard）出网，源站看到 WARP 出口 IP（见 warp.go）；
//   - direct：直连源站（可配 http/https 上游代理），源站会看到宿主机 IP。
package egress

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/atticus6/vultro-engine/internal/config"
)

// New 按配置构造出口客户端，返回的 close 负责释放资源（目前只有 WARP 的设备）。
//
// **warp 模式的失败必须冒泡**：静默退回直连会让源站看到宿主机 IP，正好违背该模式的
// 唯一目的——所以调用方收到错误时必须拒绝启动。
func New(cfg *config.Config, log *slog.Logger) (client *http.Client, closeFn func(), err error) {
	noop := func() {}

	switch cfg.EgressMode {
	case "warp":
		warp, err := StartWarpEgress(cfg.WarpIdentityPath, log)
		if err != nil {
			return nil, noop, err
		}
		return warp.HTTPClient(10 * time.Minute), warp.Close, nil

	case "direct":
		client := &http.Client{Timeout: 10 * time.Minute}
		if cfg.EgressProxy != "" {
			transport, err := proxyTransport(cfg.EgressProxy)
			if err != nil {
				return nil, noop, err
			}
			client.Transport = transport
		}
		return client, noop, nil

	default: // relay
		// relay 模式下取流走 Worker 的 fetch 端点，这个客户端不会用到；给个零值即可。
		return &http.Client{Timeout: 10 * time.Minute}, noop, nil
	}
}

// proxyTransport 为 direct 模式构造带代理的 Transport（支持 http/https 上游）。
func proxyTransport(raw string) (*http.Transport, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch parsed.Scheme {
	case "http", "https":
		return &http.Transport{Proxy: http.ProxyURL(parsed)}, nil
	default:
		// 标准库不带 socks5 支持（不引第三方依赖）；用 http 上游代理或 EGRESS_MODE=warp。
		return nil, fmt.Errorf("不支持的代理协议 %q（v1 支持 http/https 上游）", parsed.Scheme)
	}
}
