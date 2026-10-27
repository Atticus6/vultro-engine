// 内嵌 cloudflared 临时隧道（quick tunnel）：进程内起一条 `*.trycloudflare.com` 隧道，
// 把本机 `ENGINE_ADDR` 暴露给 Worker，启动后**自动上报地址**（不再需要手工配隧道）。
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
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/cloudflared/cmd/cloudflared/cliutil"
	tunnelcmd "github.com/cloudflare/cloudflared/cmd/cloudflared/tunnel"
	"github.com/urfave/cli/v2"
)

// quickTunnelURLRe 匹配 quick tunnel 分配的公网地址。
var quickTunnelURLRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// quickTunnelReadyTimeout 等待"隧道连接已注册"的上限。
// 太短会在慢网络下误判失败；太长会让引擎启动看起来卡住。
const quickTunnelReadyTimeout = 90 * time.Second

// StartQuickTunnel 在进程内启动 quick tunnel，返回可被 Worker 访问的公网地址。
//
// 返回时隧道已**完成注册**（可以立即接收请求）。隧道在后台随 ctx 存活；
// ctx 取消后 cloudflared 退出。
//
// @param localURL 本地来源，形如 `http://127.0.0.1:8787`。
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
		Version:  Version,
		Flags:    tunnelcmd.Flags(),
		Commands: tunnelcmd.Commands(),
	}
	// Init 设置全局 build info 与优雅退出通道；不调用会让日志/版本上报取到空值。
	tunnelcmd.Init(cliutil.GetBuildInfo("embedded", Version), make(chan struct{}))

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
		if err != nil && ctx.Err() == nil {
			log.Error("cloudflared 退出", "err", err)
		}
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

// stdioCapture 把进程级 stdout/stderr 接到管道，收集输出并镜像回真实 stderr。
//
// 为什么动的是 `os.Stdout/os.Stderr` 而不是某个 logger：cloudflared 的日志器在初始化时
// 就绑定标准流，只能在进程层面截获；镜像回真实 fd 是为了不吞掉排障信息。
type stdioCapture struct {
	mu     sync.Mutex
	buffer []byte
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
				capture.mu.Lock()
				capture.buffer = append(capture.buffer, buf[:n]...)
				capture.mu.Unlock()
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
