// 引擎进程入口：只做装配，不含业务逻辑。
//
// 启动顺序（每一步失败都直接退出——带病运行比不运行更糟：它会注册成可用节点）：
//  1. 解析参数与配置（**命令行 > 环境变量 > 配置文件**，见 internal/config）；
//  2. 起本地 HTTP 服务（入站只经隧道，不监听公网，见 internal/server）；
//  3. 解析公网地址：显式配置 或 内嵌 cloudflared 临时隧道（见 internal/tunnel）；
//  4. 注册到 Worker（幂等，失败重试）；
//  5. 后台心跳（刷新 lastSeenAt + 排水状态）。
//
// 各包的职责见 internal/ 下的包注释。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/atticus6/vultro-engine/internal/buildinfo"
	"github.com/atticus6/vultro-engine/internal/config"
	"github.com/atticus6/vultro-engine/internal/download"
	"github.com/atticus6/vultro-engine/internal/egress"
	"github.com/atticus6/vultro-engine/internal/server"
	"github.com/atticus6/vultro-engine/internal/tunnel"
	"github.com/atticus6/vultro-engine/internal/worker"
)

func main() {
	// `--version` / `--help` 必须在读配置之前处理：自更新会用 `新二进制 --version`
	// 做冒烟检查，这一步不能因为缺配置而失败。
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--version", "-version", "-v":
			fmt.Println(buildinfo.Version)
			return
		}
	}

	args, err := parseArgs(os.Args[1:])
	if err != nil {
		exitWithUsageError(err)
	}
	if args.showHelp {
		fmt.Print(usage)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.LoadWithOptions(args.options)
	if err != nil {
		logger.Error("配置加载失败", "err", err)
		os.Exit(1)
	}

	if args.printConfig {
		printConfig(cfg)
		return
	}
	if cfg.ConfigFile != "" {
		logger.Info("已读取配置文件", "path", cfg.ConfigFile)
	}

	// ── 1) 先起本地服务：隧道要指向它 ─────────────────────────────────────────
	workerClient := worker.NewClient(cfg.WorkerBaseURL, cfg.Token, nil, logger)

	sourceClient, closeEgress, err := egress.New(cfg, logger)
	if err != nil {
		logger.Error("出口初始化失败（拒绝以会暴露宿主机 IP 的方式启动）", "err", err)
		os.Exit(1)
	}
	defer closeEgress()

	runner := download.NewRunner(workerClient, sourceClient, cfg.EgressMode, logger)
	srv := server.New(cfg, runner, logger)
	httpServer := &http.Server{Addr: cfg.Addr, Handler: srv.Handler()}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP 服务退出", "err", err)
			os.Exit(1)
		}
	}()

	// ── 2) 解析公网地址：显式配置 或 内嵌临时隧道 ────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.UsesQuickTunnel() {
		logger.Info("未配置 ENGINE_PUBLIC_URL，启动内嵌 cloudflared 临时隧道",
			"protocol", cfg.TunnelProtocol)
		publicURL, err := tunnel.StartQuickTunnel(ctx, tunnel.LocalURL(cfg.Addr), cfg.TunnelProtocol, logger)
		if err != nil {
			logger.Error("临时隧道启动失败", "err", err)
			os.Exit(1)
		}
		// quick tunnel 仅供测试/自托管：地址每次启动变化，注册时上报给 Worker 即可。
		logger.Warn("已使用临时隧道（*.trycloudflare.com）：Cloudflare 标注其不适合生产、无 SLA")
		cfg.PublicURL = publicURL
	}

	// ── 3) 注册（幂等）：把**当前**隧道地址报给 Worker ────────────────────────
	for {
		regCtx, regCancel := context.WithTimeout(ctx, 15*time.Second)
		err := workerClient.Register(regCtx, cfg.PublicURL, cfg.Capacity)
		regCancel()
		if err == nil {
			logger.Info("已注册到 Worker", "id", cfg.ID, "publicUrl", cfg.PublicURL)
			break
		}
		logger.Warn("注册失败，10s 后重试", "err", err)
		select {
		case <-ctx.Done():
			os.Exit(1)
		case <-time.After(10 * time.Second):
		}
	}

	// ── 4) 心跳：刷新 lastSeenAt；Worker 有排队任务时会立即扫一轮派发 ─────────
	go func() {
		for {
			hbCtx, hbCancel := context.WithTimeout(ctx, 15*time.Second)
			hint, err := workerClient.Heartbeat(hbCtx, srv.Draining())
			hbCancel()
			if err != nil {
				logger.Warn("心跳失败", "err", err)
			} else if hint {
				logger.Info("有排队任务待派发")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}()

	logger.Info("引擎启动",
		"addr", cfg.Addr, "capacity", cfg.Capacity, "egress", cfg.EgressMode,
		"publicUrl", cfg.PublicURL, "version", buildinfo.Version)

	// 主 goroutine 阻塞：隧道/HTTP 服务的 fatal 分支都会走 os.Exit。
	select {}
}
