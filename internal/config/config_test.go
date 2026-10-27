// 配置解析的单测：默认值、必填项、枚举与数值校验。
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// ── 配置文件与优先级（命令行 > 环境变量 > 文件） ───────────────────────────

// writeConfig 在临时目录写一个配置文件并返回路径。
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写配置文件失败: %v", err)
	}
	return path
}

// clearEnv 清掉所有会被读取的环境变量，避免开发机上的残留影响断言。
func clearEnv(t *testing.T) {
	t.Helper()
	for key := range knownFileKeys {
		t.Setenv(key, "")
	}
	t.Setenv("ENGINE_CONFIG_FILE", "")
}

func TestLoadFromConfigFile(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
# 注释与空行都应被忽略
ENGINE_ID=off1
ENGINE_TOKEN="eng_file_token"
WORKER_BASE_URL=https://worker.example.com/
export ENGINE_CAPACITY=4
`)

	cfg, err := LoadWithOptions(Options{ConfigFile: path})
	if err != nil {
		t.Fatalf("仅靠配置文件应能启动: %v", err)
	}
	if cfg.ID != "off1" || cfg.Token != "eng_file_token" || cfg.Capacity != 4 {
		t.Fatalf("文件里的值没生效: %+v", cfg)
	}
	// 成对引号要被剥掉，尾部斜杠要裁掉。
	if cfg.Token != "eng_file_token" {
		t.Errorf("引号应被剥离，得到 %q", cfg.Token)
	}
	if cfg.WorkerBaseURL != "https://worker.example.com" {
		t.Errorf("基址尾部斜杠应被裁掉，得到 %q", cfg.WorkerBaseURL)
	}
	if cfg.ConfigFile != path {
		t.Errorf("ConfigFile 应记录实际路径，得到 %q", cfg.ConfigFile)
	}
}

func TestPrecedenceCommandLineOverEnvOverFile(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
ENGINE_ID=from-file
ENGINE_TOKEN=from-file
WORKER_BASE_URL=https://file.example.com
ENGINE_CAPACITY=3
`)

	// 环境变量盖过文件。
	t.Setenv("ENGINE_ID", "from-env")
	t.Setenv("ENGINE_CAPACITY", "5")

	// 命令行盖过环境变量。
	cfg, err := LoadWithOptions(Options{
		ConfigFile: path,
		ID:         "from-flag",
	})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.ID != "from-flag" {
		t.Errorf("命令行应优先，得到 %q", cfg.ID)
	}
	if cfg.Capacity != 5 {
		t.Errorf("环境变量应盖过文件，得到 %d", cfg.Capacity)
	}
	// 没被覆盖的项仍来自文件。
	if cfg.Token != "from-file" {
		t.Errorf("未覆盖项应回退到文件，得到 %q", cfg.Token)
	}
	if cfg.WorkerBaseURL != "https://file.example.com" {
		t.Errorf("未覆盖项应回退到文件，得到 %q", cfg.WorkerBaseURL)
	}
}

func TestConfigFileErrors(t *testing.T) {
	clearEnv(t)

	// 显式指定的文件不存在 → 报错（拼错路径不能静默跑默认值）。
	if _, err := LoadWithOptions(Options{ConfigFile: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("显式指定的配置文件不存在应报错")
	}

	// 未知键 → 报错（拼错的键不能静默忽略）。
	path := writeConfig(t, "ENGINE_ID=off1\nENGINE_TOKN=typo\n")
	if _, err := LoadWithOptions(Options{ConfigFile: path}); err == nil {
		t.Fatal("未知配置项应报错")
	}

	// 不是 KEY=VALUE → 报错。
	path = writeConfig(t, "ENGINE_ID off1\n")
	if _, err := LoadWithOptions(Options{ConfigFile: path}); err == nil {
		t.Fatal("非 KEY=VALUE 行应报错")
	}
}

func TestMissingRequiredMentionsAllSources(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "ENGINE_ID=off1\n")
	_, err := LoadWithOptions(Options{ConfigFile: path})
	if err == nil {
		t.Fatal("缺令牌应报错")
	}
	// 报错要告诉管理员"三处都没给"，而不是只提环境变量（他可能只用了文件）。
	if !strings.Contains(err.Error(), "命令行参数") || !strings.Contains(err.Error(), "配置文件") {
		t.Errorf("报错应说明三处来源都试过，得到 %v", err)
	}
}
