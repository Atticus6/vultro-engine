// 命令行参数解析：只覆盖配置项，不做子命令。
//
// 为什么手写而不是引 `flag`/`cobra`：参数全是"键=值"的扁平覆盖（十个），
// `flag` 的 `-x` 单横线风格与 README 里的 `--engine-id` 不一致，引 cobra 又
// 会为一个单命令二进制加一层命令树。这里只做两件事：识别 `--help`/`--version`，
// 与把已知参数收进 `config.Options`。
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/atticus6/vultro-engine/internal/buildinfo"
	"github.com/atticus6/vultro-engine/internal/config"
)

// usage 是 `--help` 的输出。
//
// 参数名刻意与配置文件/环境变量一一对应（`--engine-id` ↔ `ENGINE_ID`），
// 管理员在三处之间切换时不需要另记一套名字。
const usage = `vultro-engine — 离线下载引擎

用法：
  vultro-engine [参数]

参数（优先级：命令行 > 环境变量 > 配置文件）：
  --engine-id <id>          节点 id，如 off1（ENGINE_ID）
  --token <eng_…>           该节点专属令牌（ENGINE_TOKEN）
  --worker-url <url>        Worker 基址（WORKER_BASE_URL）
  --public-url <url>        外部隧道地址；留空 = 自建临时隧道（ENGINE_PUBLIC_URL）
  --addr <host:port>        本地监听地址，默认 :8787（ENGINE_ADDR）
  --capacity <1-16>         并发槽位，默认 2（ENGINE_CAPACITY）
  --egress <mode>           relay / warp / direct，默认 relay（EGRESS_MODE）
  --egress-proxy <url>      仅 direct：http/https 上游代理（EGRESS_PROXY）
  --tunnel-protocol <p>     http2 / quic / auto，默认 http2（TUNNEL_PROTOCOL）
  --warp-identity <dir>     WARP 身份目录（WARP_IDENTITY_PATH）
  --config <path>           配置文件路径（默认 ~/.config/vultro-engine/config）
  --print-config            打印生效的配置（令牌打码）后退出
  -h, --help                显示本帮助
  -v, --version             显示版本

配置文件格式（KEY=VALUE，可被 source / systemd EnvironmentFile 直接使用）：
  ENGINE_ID=off1
  ENGINE_TOKEN=eng_…
  WORKER_BASE_URL=https://example.com
`

// parsedArgs 解析结果。
type parsedArgs struct {
	options     config.Options
	showHelp    bool
	printConfig bool
}

// parseArgs 解析 `os.Args[1:]`。
//
// 支持 `--key=value` 与 `--key value` 两种写法：前者是脚本里最常见的形态
// （`install.sh` 拼命令时不会因为值里有空格而拆错），后者便于手工敲。
func parseArgs(args []string) (parsedArgs, error) {
	result := parsedArgs{}

	// 值型参数：flag 名 → 目标字段。
	setters := map[string]*string{
		"--engine-id":       &result.options.ID,
		"--token":           &result.options.Token,
		"--worker-url":      &result.options.WorkerBaseURL,
		"--public-url":      &result.options.PublicURL,
		"--addr":            &result.options.Addr,
		"--capacity":        &result.options.Capacity,
		"--egress":          &result.options.EgressMode,
		"--egress-proxy":    &result.options.EgressProxy,
		"--tunnel-protocol": &result.options.TunnelProtocol,
		"--warp-identity":   &result.options.WarpIdentityPath,
		"--config":          &result.options.ConfigFile,
	}

	for index := 0; index < len(args); index++ {
		arg := args[index]

		switch arg {
		case "-h", "--help":
			result.showHelp = true
			return result, nil
		case "--print-config":
			result.printConfig = true
			continue
		}

		name, inlineValue, hasInline := strings.Cut(arg, "=")
		target, ok := setters[name]
		if !ok {
			return result, fmt.Errorf("未知参数 %q（--help 查看用法）", arg)
		}
		if hasInline {
			*target = inlineValue
			continue
		}
		// 没有内联值时取下一个参数；缺值要明确报错，否则会把 `--token` 后面的
		// 另一个参数当成它的值，得到一个"看起来合法但不对"的配置。
		if index+1 >= len(args) {
			return result, fmt.Errorf("参数 %s 需要一个值", name)
		}
		index++
		*target = args[index]
	}

	return result, nil
}

// printConfig 打印生效配置（令牌打码）。
//
// 排障用：管理员经常分不清"这个值到底来自文件、环境变量还是命令行"，
// 把来源与最终值一起打出来比让他逐处 grep 快得多。
func printConfig(cfg *config.Config) {
	source := cfg.ConfigFile
	if source == "" {
		source = "（未读取配置文件）"
	}
	fmt.Printf("version:             %s\n", buildinfo.Version)
	fmt.Printf("config file:         %s\n", source)
	fmt.Printf("ENGINE_ID:           %s\n", cfg.ID)
	fmt.Printf("ENGINE_TOKEN:        %s\n", maskToken(cfg.Token))
	fmt.Printf("WORKER_BASE_URL:     %s\n", cfg.WorkerBaseURL)
	fmt.Printf("ENGINE_PUBLIC_URL:   %s\n", cfg.PublicURL)
	fmt.Printf("ENGINE_ADDR:         %s\n", cfg.Addr)
	fmt.Printf("ENGINE_CAPACITY:     %d\n", cfg.Capacity)
	fmt.Printf("EGRESS_MODE:         %s\n", cfg.EgressMode)
	fmt.Printf("EGRESS_PROXY:        %s\n", cfg.EgressProxy)
	fmt.Printf("TUNNEL_PROTOCOL:     %s\n", cfg.TunnelProtocol)
	fmt.Printf("WARP_IDENTITY_PATH:  %s\n", cfg.WarpIdentityPath)
}

// maskToken 只留前缀与末四位：足够确认"是不是这一枚"，又不至于把凭据打进日志。
func maskToken(token string) string {
	if token == "" {
		return "（未设置）"
	}
	if len(token) <= 12 {
		return "***"
	}
	return token[:8] + "…" + token[len(token)-4:]
}

// exitWithUsageError 打印用法错误并以状态码 2 退出（与常见 CLI 约定一致）。
func exitWithUsageError(err error) {
	fmt.Fprintf(os.Stderr, "参数错误：%v\n", err)
	os.Exit(2)
}
