// Command server 是 newx-gateway 统一入口的进程入口。
//
// 职责边界：加载配置 → 构造服务 → 监听 → 优雅退出。
// 不包含任何路由、中间件与业务逻辑，那些都在 internal 下。
//
// 退出码约定：
//
//	0  正常退出（收到信号并优雅关闭）
//	1  运行期失败（端口被占用、监听失败等）
//	2  配置或启动参数非法
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"newx-gateway/internal/app"
	"newx-gateway/internal/config"
	"newx-gateway/internal/observability"
)

// version 可在构建期通过 -ldflags "-X main.version=1.2.3" 注入。
var version = "dev"

const (
	exitOK          = 0
	exitRuntimeFail = 1
	exitConfigFail  = 2
)

func main() {
	os.Exit(run())
}

func run() int {
	envFile := flag.String("env-file", "", "从 KEY=VALUE 文件加载环境变量（已存在的进程变量优先，不会被覆盖）")
	showVersion := flag.Bool("version", false, "打印版本号并退出")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return exitOK
	}

	if *envFile != "" {
		applied, skipped, err := config.LoadEnvFile(*envFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "加载 env 文件失败: %v\n", err)
			return exitConfigFail
		}
		fmt.Fprintf(os.Stderr, "env 文件 %s: 注入 %d 项，跳过 %d 项（进程环境变量优先）\n", *envFile, applied, skipped)
	}

	cfg, err := config.Load()
	if err != nil {
		// 配置错误必须一次性全部打印：分批报错会让部署来回试错。
		fmt.Fprintf(os.Stderr, "配置校验失败:\n%v\n", err)
		return exitConfigFail
	}
	// 构建期注入的版本优先于环境变量：构建产物的身份不应被运行环境改写。
	if version == "dev" && cfg.Version != "" {
		version = cfg.Version
	}
	cfg.Version = version

	logger, err := observability.NewLogger(cfg.LogLevel, cfg.LogFormat, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化日志失败: %v\n", err)
		return exitConfigFail
	}

	gateway, err := app.New(cfg, logger)
	if err != nil {
		logger.Error("装配入口失败，进程退出", "err", err)
		return exitRuntimeFail
	}

	logger.Info("newx-gateway 启动",
		"version", cfg.Version,
		"env", cfg.Env,
		"addr", cfg.HTTPAddr,
		"modules", gateway.Modules(),
		"apps", gateway.Registry.Codes(),
		"mcp_enabled", cfg.MCPEnabled,
		"openapi_enabled", cfg.OpenAPIEnabled,
	)

	runErr := make(chan error, 1)
	go func() { runErr <- gateway.Run() }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	select {
	case err := <-runErr:
		// 监听失败（例如端口被占用）必须快速失败，不能静默重试。
		if err != nil {
			logger.Error("监听失败，进程退出", "addr", cfg.HTTPAddr, "err", err)
			return exitRuntimeFail
		}
		logger.Info("服务已停止")
		return exitOK

	case received := <-signals:
		logger.Info("收到退出信号，开始优雅退出", "signal", received.String(), "wait", cfg.ExitWaitTime.String())

		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ExitWaitTime)
		defer cancel()

		if err := gateway.Shutdown(shutdownCtx); err != nil {
			logger.Error("优雅退出失败，进程退出", "err", err)
			return exitRuntimeFail
		}

		// 等待监听循环真正返回，避免退出过快截断响应。
		select {
		case <-runErr:
		case <-time.After(cfg.ExitWaitTime):
			logger.Warn("等待在途请求超时，强制退出", "wait", cfg.ExitWaitTime.String())
		}

		logger.Info("已优雅退出")
		return exitOK
	}
}
