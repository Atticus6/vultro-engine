// 配置文件解析：`KEY=VALUE` 行格式，与引擎的环境变量同名。
//
// 为什么用 env 文件的形状而不是 TOML/YAML：install.sh 写的就是这几行，管理员
// 手工改、`source` 进 shell、塞进 systemd 的 `EnvironmentFile=` 都直接可用；
// 换一种格式只会多一份"这个字段叫什么"的翻译。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 配置文件里允许出现的键（与 `Load` 读取的环境变量同名）。
//
// 白名单而不是"照单全收"：拼错一个键（`ENGINE_TOKN`）时应当报错，而不是让引擎
// 带着空令牌启动、在注册时拿一个看不懂的 401。
var knownFileKeys = map[string]bool{
	"ENGINE_ID":          true,
	"ENGINE_TOKEN":       true,
	"WORKER_BASE_URL":    true,
	"ENGINE_PUBLIC_URL":  true,
	"ENGINE_ADDR":        true,
	"ENGINE_CAPACITY":    true,
	"EGRESS_MODE":        true,
	"EGRESS_PROXY":       true,
	"TUNNEL_PROTOCOL":    true,
	"WARP_IDENTITY_PATH": true,
}

// defaultConfigPath 默认配置文件位置（`--config` / `ENGINE_CONFIG_FILE` 可覆盖）。
//
// 放用户目录而不是 `/etc`：install.sh 不需要 root 也能写；容器里没人会挂这个文件，
// 那边继续用环境变量（优先级更高，不受影响）。
func defaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "vultro-engine", "config")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "vultro-engine", "config")
}

// parseConfigFile 读取 `KEY=VALUE` 文件。
//
// 支持空行、`#` 注释、`export ` 前缀与成对引号——这样同一个文件也能被
// `source` 或 systemd 的 EnvironmentFile 直接使用。
//
// @return 键值表；文件不存在时由调用方决定是否算错（默认路径缺失是正常的）。
func parseConfigFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	values := make(map[string]string)
	for index, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s 第 %d 行不是 KEY=VALUE 形式", path, index+1)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// 成对引号才剥：值里本来就带引号（少见）时保持原样。
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if !knownFileKeys[key] {
			return nil, fmt.Errorf("%s 第 %d 行有未知配置项 %q", path, index+1, key)
		}
		values[key] = value
	}
	return values, nil
}
