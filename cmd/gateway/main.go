package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/config"
	"github.com/nidjbs/go-ai-gateway/internal/gateway"
	"github.com/nidjbs/go-ai-gateway/internal/tracing"
)

func main() {
	configPath := flag.String("config", "configs/config.yaml", "configuration file")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := tracing.Init(ctx, cfg.Tracing)
	if err != nil {
		log.Fatal(err)
	}
	if shutdownTracing != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdownTracing(shutdownCtx); err != nil {
				logger.Warn("tracing shutdown failed", "error", err)
			}
		}()
	}

	if err := gateway.Run(ctx, cfg, *configPath, logger); err != nil {
		log.Fatal(err)
	}
}
