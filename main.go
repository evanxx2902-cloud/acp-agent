package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"github.com/coder/acp-go-sdk"
)

func main() {
	cfg := LoadConfig()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	store, err := NewStore(cfg.DBPath)
	if err != nil {
		slog.Error("failed to open session store", "path", cfg.DBPath, "error", err)
		os.Exit(1)
	}
	defer store.Close()
	slog.Info("session store opened", "path", cfg.DBPath)

	chatModel, err := NewChatModel(ctx, cfg)
	if err != nil {
		slog.Error("failed to create chat model", "error", err)
		os.Exit(1)
	}

	ag := NewEinoAgent(cfg, chatModel, store)
	conn := acp.NewAgentSideConnection(ag, os.Stdout, os.Stdin)
	conn.SetLogger(logger)
	ag.SetAgentConnection(conn)

	slog.Info("agent server started", "version", acp.ProtocolVersionNumber)

	<-conn.Done()
	slog.Info("agent server shutting down")
}
