// Command server 启动 doubao2api：对外 OpenAI 兼容，内部转发豆包 /chat/completion。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"doubao2api/internal/config"
	"doubao2api/internal/server"
)

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(cfg),
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("doubao2api 监听 %s（上游 %s，账号 %d，工作模式 %v，下游鉴权 %v）",
			cfg.Listen, cfg.Upstream.ChatURL, len(cfg.Accounts), cfg.WorkMode.Enabled, len(cfg.APIKeys) > 0)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("收到退出信号，正在关闭…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
