// 引擎配置：环境变量是唯一配置面（单二进制 + systemd/容器的部署形态）。
//
// 必需项缺失时启动即失败（Fail Fast）——带病运行的引擎比不运行更糟：
// 它会注册成可用节点，让 Worker 把任务派发给一个注定失败的地方。
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Version 引擎版本（注册/心跳上报；滚动升级时用于判断哪台还在跑旧版）。
// 构建时可用 -ldflags 覆盖：go build -ldflags "-X main.Version=v1.0.1"。
var Version = "dev"

// Config 引擎运行配置（全部来自环境变量，见 README 的变量表）。
type Config struct {
	// ID 引擎 id（如 off1）：Worker 侧派发路由的键，必须稳定且唯一。
	ID string

	// PublicURL 引擎的公网地址。
	//
	// **留空 = 启动时自动开一条 cloudflared 临时隧道（quick tunnel）**，地址每次启动
	// 随机（`*.trycloudflare.com`），注册时上报给 Worker；非空 = 使用外部已有隧道
	// （named tunnel / 自建反代），此时引擎不会内嵌 cloudflared。
	PublicURL string

	// TunnelProtocol quick tunnel 的连接协议：`http2`（默认，TCP 7844，最稳）、
	// `quic`（UDP 7844，延迟更好但被不少机房/网络封）或 `auto`（先 quic 再回落）。
	TunnelProtocol string

	// Token 该节点**专属**的引擎令牌（`eng_…`）。
	// 双向 Bearer：引擎调 Worker 的内部端点、Worker 派发到引擎，都用它。
	// 由管理员在后台「离线下载节点」页创建/重置节点时生成——每台机器一枚，
	// 没有全局共享 secret（一台泄露不影响其它节点）。
	Token string

	// WorkerBaseURL Worker 基址（生产/测试各自的域名；本地联调 http://localhost:8787）。
	WorkerBaseURL string

	// Capacity 并发槽位（同时下载的任务数）。每任务一片 ≤64 MiB 缓冲。
	Capacity int

	// Addr 本地监听地址（默认 :8787）。只给内嵌隧道/本机访问，不暴露公网端口。
	Addr string

	// EgressMode 取源站的出口：
	// - `relay`（默认）：经 Worker 转发，源站只见 Cloudflare IP，引擎无需额外配置；
	// - `warp`：引擎经 WARP（用户态 WireGuard）出网，源站见到的是 WARP 出口 IP；
	// - `direct`：直连源站（可配 EgressProxy），源站会看到宿主机 IP。
	EgressMode string

	// EgressProxy 仅 `direct` 模式：http/https 上游代理（如宿主机上的 WARP 客户端）。
	EgressProxy string

	// WarpIdentityPath WARP 身份的持久化路径（目录）。
	//
	// 复用同一身份避免每次重启都向 Cloudflare 注册新设备（有频控）。容器里应挂 volume。
	WarpIdentityPath string
}

// LoadConfig 从环境变量读取配置；必需项缺失返回错误。
func LoadConfig() (*Config, error) {
	cfg := &Config{
		ID:               os.Getenv("ENGINE_ID"),
		PublicURL:        strings.TrimRight(os.Getenv("ENGINE_PUBLIC_URL"), "/"),
		TunnelProtocol:   strings.ToLower(envDefault("TUNNEL_PROTOCOL", "http2")),
		Token:            os.Getenv("ENGINE_TOKEN"),
		WorkerBaseURL:    strings.TrimRight(os.Getenv("WORKER_BASE_URL"), "/"),
		Addr:             envDefault("ENGINE_ADDR", ":8787"),
		EgressMode:       strings.ToLower(envDefault("EGRESS_MODE", "relay")),
		EgressProxy:      os.Getenv("EGRESS_PROXY"),
		WarpIdentityPath: envDefault("WARP_IDENTITY_PATH", "./warp-identity"),
	}

	for name, value := range map[string]string{
		"ENGINE_ID":       cfg.ID,
		"ENGINE_TOKEN":    cfg.Token,
		"WORKER_BASE_URL": cfg.WorkerBaseURL,
	} {
		if value == "" {
			return nil, fmt.Errorf("缺少必需环境变量 %s", name)
		}
	}

	switch cfg.TunnelProtocol {
	case "http2", "quic", "auto":
	default:
		return nil, fmt.Errorf("TUNNEL_PROTOCOL 只支持 http2 / quic / auto，得到 %q", cfg.TunnelProtocol)
	}

	switch cfg.EgressMode {
	case "relay", "warp", "direct":
	default:
		return nil, fmt.Errorf("EGRESS_MODE 只支持 relay / warp / direct，得到 %q", cfg.EgressMode)
	}

	cfg.Capacity = 2
	if raw := os.Getenv("ENGINE_CAPACITY"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 16 {
			return nil, fmt.Errorf("ENGINE_CAPACITY 必须是 1-16 的整数，得到 %q", raw)
		}
		cfg.Capacity = n
	}

	return cfg, nil
}

// usesQuickTunnel 是否使用内嵌 cloudflared 临时隧道（未显式提供公网地址时）。
func (c *Config) usesQuickTunnel() bool {
	return c.PublicURL == ""
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
