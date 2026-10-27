// 配置解析：**命令行参数 > 环境变量 > 配置文件**，三级依次回退。
//
// 环境变量与配置文件同名同义（见 README 的变量表）；命令行参数是它们的
// 短名字（`--engine-id` / `--token` / …），只覆盖对应的那一项。
//
// 为什么是这个优先级：命令行最"显式"（只影响这一次启动，调试/临时接管用），
// 环境变量次之（容器与 systemd 的常规形态），文件最弱（落盘的默认值，install.sh
// 会写一份）。这样 `vultro-engine --token=xxx` 能临时盖掉文件里的旧令牌，而不用
// 去改文件。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config 引擎运行配置（来源见包注释的优先级）。
type Config struct {
	// ID 引擎 id（如 off1）：仅用于本地日志与运维对账，**不是身份**。
	// 身份完全由令牌决定（见 Worker 侧注册/心跳契约）。
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

	// ConfigFile 本次实际读取的配置文件路径（空 = 没有读到任何文件）。
	// 只用于启动日志与 `--print-config`，不参与业务。
	ConfigFile string
}

// Options 命令行参数（零值 = 未指定，此时回退到环境变量 / 文件）。
type Options struct {
	// ConfigFile `--config`：显式指定配置文件。
	ConfigFile string

	ID               string
	Token            string
	WorkerBaseURL    string
	PublicURL        string
	Addr             string
	Capacity         string
	EgressMode       string
	EgressProxy      string
	TunnelProtocol   string
	WarpIdentityPath string
}

// Load 用默认参数（仅环境变量 + 配置文件）加载配置。
func Load() (*Config, error) {
	return LoadWithOptions(Options{})
}

// LoadWithOptions 按 `命令行 > 环境变量 > 配置文件` 解析配置。
//
// 必需项缺失或取值非法时返回错误（Fail Fast）——带病运行的引擎比不运行更糟：
// 它会注册成可用节点，让 Worker 把任务派发给一个注定失败的地方。
func LoadWithOptions(opts Options) (*Config, error) {
	filePath := opts.ConfigFile
	if filePath == "" {
		filePath = envDefault("ENGINE_CONFIG_FILE", defaultConfigPath())
	}

	fromFile := map[string]string{}
	configFile := ""
	if filePath != "" {
		values, err := parseConfigFile(filePath)
		switch {
		case err == nil:
			fromFile = values
			configFile = filePath
		case os.IsNotExist(err):
			// 默认路径下没有文件是正常的（纯环境变量部署）；但**显式指定**的文件
			// 不存在要报错，否则 `--config` 拼错会静默跑在默认值上。
			if opts.ConfigFile != "" || os.Getenv("ENGINE_CONFIG_FILE") != "" {
				return nil, fmt.Errorf("配置文件 %s 不存在", filePath)
			}
		default:
			return nil, fmt.Errorf("读取配置文件 %s 失败：%w", filePath, err)
		}
	}

	// 三级取值：命令行 → 环境变量 → 文件 → 内置默认。
	pick := func(flagValue, envKey, fallback string) string {
		if flagValue != "" {
			return flagValue
		}
		if value := os.Getenv(envKey); value != "" {
			return value
		}
		if value := fromFile[envKey]; value != "" {
			return value
		}
		return fallback
	}

	cfg := &Config{
		ID:               pick(opts.ID, "ENGINE_ID", ""),
		PublicURL:        strings.TrimRight(pick(opts.PublicURL, "ENGINE_PUBLIC_URL", ""), "/"),
		TunnelProtocol:   strings.ToLower(pick(opts.TunnelProtocol, "TUNNEL_PROTOCOL", "http2")),
		Token:            pick(opts.Token, "ENGINE_TOKEN", ""),
		WorkerBaseURL:    strings.TrimRight(pick(opts.WorkerBaseURL, "WORKER_BASE_URL", ""), "/"),
		Addr:             pick(opts.Addr, "ENGINE_ADDR", ":8787"),
		EgressMode:       strings.ToLower(pick(opts.EgressMode, "EGRESS_MODE", "relay")),
		EgressProxy:      pick(opts.EgressProxy, "EGRESS_PROXY", ""),
		WarpIdentityPath: pick(opts.WarpIdentityPath, "WARP_IDENTITY_PATH", "./warp-identity"),
		ConfigFile:       configFile,
	}

	for name, value := range map[string]string{
		"ENGINE_ID":       cfg.ID,
		"ENGINE_TOKEN":    cfg.Token,
		"WORKER_BASE_URL": cfg.WorkerBaseURL,
	} {
		if value == "" {
			return nil, fmt.Errorf("缺少必需配置 %s（命令行参数 / 环境变量 / 配置文件 %s 都没有）", name, displayPath(filePath))
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

	capacityRaw := pick(opts.Capacity, "ENGINE_CAPACITY", "")
	cfg.Capacity = 2
	if capacityRaw != "" {
		n, err := strconv.Atoi(capacityRaw)
		if err != nil || n < 1 || n > 16 {
			return nil, fmt.Errorf("ENGINE_CAPACITY 必须是 1-16 的整数，得到 %q", capacityRaw)
		}
		cfg.Capacity = n
	}

	return cfg, nil
}

// UsesQuickTunnel 是否使用内嵌 cloudflared 临时隧道（未显式提供公网地址时）。
func (c *Config) UsesQuickTunnel() bool {
	return c.PublicURL == ""
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// displayPath 报错时用的路径文案（空值说明"没有默认路径"）。
func displayPath(path string) string {
	if path == "" {
		return "（无默认路径）"
	}
	return path
}
