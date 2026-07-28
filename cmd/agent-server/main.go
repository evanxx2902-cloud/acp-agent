package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"github.com/coder/acp-go-sdk"

	"acp/internal/agent"
	"acp/internal/config"
	"acp/internal/llm"
)

func main() {
	cfg := config.Load()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	// Initialize SQLite session store
	store, err := agent.NewStore(cfg.DBPath)
	if err != nil {
		slog.Error("failed to open session store", "path", cfg.DBPath, "error", err)
		os.Exit(1)
	}
	defer store.Close()
	slog.Info("session store opened", "path", cfg.DBPath)

	// Create the chat model
	chatModel, err := llm.NewChatModel(ctx, cfg)
	if err != nil {
		slog.Error("failed to create chat model", "error", err)
		os.Exit(1)
	}

	// Create the ACP agent with the chat model and session store
	ag := agent.NewEinoAgent(cfg, chatModel, store)

	// Create the agent-side connection (stdio transport)
	conn := acp.NewAgentSideConnection(ag, os.Stdout, os.Stdin)
	conn.SetLogger(logger)

	// Set the connection reference on the agent (AgentConnAware pattern)
	ag.SetAgentConnection(conn)

	slog.Info("agent server started", "version", acp.ProtocolVersionNumber)

	// Block until the peer disconnects
	<-conn.Done()
	slog.Info("agent server shutting down")
}
