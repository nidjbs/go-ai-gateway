package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"example.com/light-llm-gateway/internal/config"
	"example.com/light-llm-gateway/internal/gateway"
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
	if err := gateway.Run(ctx, cfg, logger); err != nil {
		log.Fatal(err)
	}
}
