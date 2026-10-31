// Package tunnel 在进程内起一条 cloudflared 临时隧道（quick tunnel），
// 把本机 HTTP 服务暴露成 `https://*.trycloudflare.com`，启动后自动上报地址
// 给 Worker（不再需要手工配隧道）。
//
// ## 为什么是"内嵌 CLI 命令图"而不是某个 SDK 函数
//
// cloudflared **没有公开的 quick tunnel 构造函数**：CLI 里的 `RunQuickTunnel` 参数类型
// 不导出（cloudflare/cloudflared#986），`StartServer` 又要求一个已初始化好的
// `*cli.Context` 与全局 config。因此这里采用它自己 CLI 的命令图（`tunnel.Commands()`）
// 在本进程内运行——仍然是同一个二进制、同一条官方代码路径，只是没有外部进程。
//
// ## 地址从哪里来
//
// cloudflared 把 "Your quick Tunnel has been created! Visit it at https://xxx.trycloudflare.com"
// 打到 stderr。这里把进程的 stdout/stderr 接成管道，正则扫出地址；并且**要等到**
// `Registered tunnel connection` 才算就绪（地址先打印、连接后建立，早返回会 502）。
//
// ## 已知限制（必须在 README/后台说明）
//
//   - quick tunnel 每次启动换地址，且 Cloudflare 明确标注"仅供测试"、无 SLA；
//   - 连接协议默认 `http2`（TCP 7844）：不少机房/网络封 UDP，`quic` 会一直
//     "no recent network activity"；`auto` 留给网络条件好的部署；
//   - 隧道断开时进程退出（由 systemd/容器重启，重启后地址与注册都会刷新）。
package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/cloudflared/cmd/cloudflared/cliutil"
	tunnelcmd "github.com/cloudflare/cloudflared/cmd/cloudflared/tunnel"
	"github.com/urfave/cli/v2"

	"github.com/atticus6/vultro-engine/internal/buildinfo"
)

// quickTunnelURLRe 匹配 quick tunnel 分配的公网地址。
var quickTunnelURLRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// quickTunnelReadyTimeout 等待"隧道连接已注册"的上限。
// 太短会在慢网络下误判失败；太长会让引擎启动看起来卡住。
const quickTunnelReadyTimeout = 90 * time.Second

// LocalURL 把监听地址转成隧道应当访问的本机 URL。
//
// 监听在通配地址（`:8787` / `0.0.0.0:8787` / `[::]:8787`）时用 127.0.0.1 访问：
// 部分环境里通配地址不是可拨号地址。显式指定了主机则原样保留。
func LocalURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// 不含端口的畸形值：保持"看起来像个地址"的原样行为，交给 ListenAndServe 报错。
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// StartQuickTunnel 在进程内启动 quick tunnel，返回可被 Worker 访问的公网地址。
//
// 返回时隧道已**完成注册**（可以立即接收请求）。隧道在后台随 ctx 存活；
// ctx 取消后 cloudflared 退出。
//
// @param localURL 本地来源，形如 `http://127.0.0.1:8787`（见 LocalURL）。
// @param protocol `http2` / `quic` / `auto`，透传 cloudflared 的 `--protocol`。
func StartQuickTunnel(ctx context.Context, localURL, protocol string, log *slog.Logger) (string, error) {
	if protocol == "" {
		protocol = "http2"
	}

	// 接管标准流并**不恢复**：cloudflared 的日志器在构造时绑定 fd，恢复会让后续日志
	// 写进已关闭的管道。捕获器把内容同时镜像回真实 stderr，排障信息不会丢。
	capture := newStdioCapture()

	app := &cli.App{
		Name:     "cloudflared",
		Version:  buildinfo.Version,
		Flags:    tunnelcmd.Flags(),
		Commands: tunnelcmd.Commands(),
	}
	// Init 设置全局 build info 与优雅退出通道；不调用会让日志/版本上报取到空值。
	tunnelcmd.Init(cliutil.GetBuildInfo("embedded", buildinfo.Version), make(chan struct{}))

	/*
	 * **不要**在函数返回时 cancel：隧道必须活到进程退出。
	 * 这里直接用调用方 ctx（main 里是 context.Background 派生的长生命周期 ctx），
	 * 早先写成 `defer cancel()` 导致一返回就 shutdown、隧道立刻 530。
	 */
	go func() {
		err := app.RunContext(ctx, []string{
			"cloudflared", "tunnel",
			"--url", localURL,
			"--protocol", protocol,
			"--no-autoupdate",
		})
		stopTunnelOnStop(ctx, err, log, os.Exit)
	}()

	deadline := time.Now().Add(quickTunnelReadyTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		out := capture.String()
		if url := quickTunnelURLRe.FindString(out); url != "" {
			if strings.Contains(out, "Registered tunnel connection") {
				log.Info("内嵌隧道已就绪", "url", url, "protocol", protocol)
				return url, nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}

	return "", fmt.Errorf("quick tunnel 在 %s 内未就绪（多半是 UDP/TCP 出网被封，可试 TUNNEL_PROTOCOL=quic）", quickTunnelReadyTimeout)
}

// stopTunnelOnStop 隧道进程停止后的收尾：ctx 正常取消（进程关闭）时什么也不做；
// 其余情况一律以 1 退出，由守护进程重启。
//
// ## 为什么必须退出，而不是只记一条日志
//
// 没有隧道的引擎在 Worker 眼里仍然"在线"：心跳照发、槽位照占，Worker 会把任务派到
// 一个不可达的地址（每次都要等引擎失联超时才改派）。`RunContext` 可能以 error 返回、
// 也可能"正常"返回（内部 shutdown），两种都意味着隧道已经不可用。
//
// `exit` 抽成参数是为了单测能断言"退出了、退出码是 1"而不真的杀掉测试进程。
func stopTunnelOnStop(ctx context.Context, runErr error, log *slog.Logger, exit func(int)) {
	if ctx.Err() != nil {
		return // 进程正在关闭：这是预期的退出路径
	}
	if runErr != nil {
		log.Error("cloudflared 退出", "err", runErr)
	} else {
		log.Error("cloudflared 已退出（RunContext 无错误返回）")
	}
	log.Error("内嵌隧道已终止：退出进程，交由守护进程重启（否则 Worker 会把任务派到不可达地址）")
	exit(1)
}

// maxCapturedBytes 捕获缓冲的上限（保留最近的一段）。
//
// 为什么需要上限：`os.Stdout/os.Stderr` 在进程存活期间一直被接到管道，而就绪检测
// 只需要启动阶段的那几行（地址 + Registered tunnel connection）。不设上限时，cloudflared
// 的每条日志都会永久留在内存里，守护进程跑得越久涨得越多。
const maxCapturedBytes = 64 << 10

// stdioCapture 把进程级 stdout/stderr 接到管道，收集输出并镜像回真实 stderr。
//
// 为什么动的是 `os.Stdout/os.Stderr` 而不是某个 logger：cloudflared 的日志器在初始化时
// 就绑定标准流，只能在进程层面截获；镜像回真实 fd 是为了不吞掉排障信息。
type stdioCapture struct {
	mu     sync.Mutex
	buffer []byte
}

// appendOutput 追加一段输出，并裁剪到最近 maxCapturedBytes 字节。
//
// 裁剪在**原数组内**原地拷贝（而不是重新分配）：稳态下每来一块日志就裁一次，
// 反复分配 64 KiB 会造成没必要的 GC 压力；原地拷完 len 固定为上限，cap 最多涨到
// 上限的两倍左右。
func (c *stdioCapture) appendOutput(chunk []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buffer = append(c.buffer, chunk...)
	if len(c.buffer) > maxCapturedBytes {
		copy(c.buffer, c.buffer[len(c.buffer)-maxCapturedBytes:])
		c.buffer = c.buffer[:maxCapturedBytes]
	}
}

func newStdioCapture() *stdioCapture {
	capture := &stdioCapture{}
	realStderr := os.Stderr

	reader, writer, err := os.Pipe()
	if err != nil {
		// 管道都建不起来时退化为"不捕获"，隧道仍可工作（只是拿不到地址）。
		return capture
	}
	os.Stdout = writer
	os.Stderr = writer

	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := reader.Read(buf)
			if n > 0 {
				capture.appendOutput(buf[:n])
				_, _ = realStderr.Write(buf[:n])
			}
			if readErr != nil {
				return
			}
		}
	}()

	return capture
}

func (c *stdioCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buffer)
}
