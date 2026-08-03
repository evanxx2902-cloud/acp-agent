package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

	"acp/ent"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"

	"github.com/coder/acp-go-sdk"
	"github.com/cloudwego/eino/callbacks"
)

func main() {
	cfg := LoadConfig()

	level := slog.LevelInfo
	switch cfg.Log.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	// Open Ent client
	dsn := cfg.Data.Database.DSN
	driver := cfg.Data.Database.Driver

	// Ensure data directory exists
	if strings.HasPrefix(dsn, "file:") {
		path := strings.TrimPrefix(dsn, "file:")
		if idx := strings.Index(path, "?"); idx >= 0 {
			path = path[:idx]
		}
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			slog.Error("failed to create data directory", "dir", dir, "error", err)
			os.Exit(1)
		}
	}

	// For modernc.org/sqlite, use _pragma query params for foreign keys
	// Ent requires _fk=1 or _pragma=foreign_keys(ON) in the DSN
	entClient, err := ent.Open(driver, dsn)
	if err != nil {
		slog.Error("failed to open database", "driver", driver, "dsn", dsn, "error", err)
		os.Exit(1)
	}
	defer entClient.Close()

	// Run auto-migration
	if err := entClient.Schema.Create(ctx); err != nil {
		slog.Error("failed to create schema", "error", err)
		os.Exit(1)
	}

	// Initialize global session manager
	sessionMgr := NewSessionManager(entClient)
	_ = sessionMgr

	// Setup LLM provider
	llmConfigPath := os.Getenv("ACP_LLM_CONFIG_PATH")
	if llmConfigPath == "" {
		// Look for legacy config.json
		if _, err := os.Stat("config.json"); err == nil {
			llmConfigPath = "config.json"
		}
	}
	llmProvider := NewDefaultLLMConfigProvider(llmConfigPath)
	modelInfo := &DefaultModelInfoProvider{}

	// Global callbacks
	callbacks.AppendGlobalHandlers(newAgentCallback())

	addr := cfg.GetListenAddr()
	switch {
	case strings.HasPrefix(addr, "tcp://"):
		tcpAddr := strings.TrimPrefix(addr, "tcp://")
		serveTCP(ctx, tcpAddr, cfg, llmProvider, modelInfo, entClient, logger)
	case strings.HasPrefix(addr, "unix://"):
		unixPath := strings.TrimPrefix(addr, "unix://")
		serveUnix(ctx, unixPath, cfg, llmProvider, modelInfo, entClient, logger)
	default:
		serveStdio(cfg, llmProvider, modelInfo, entClient, logger)
	}

	slog.Info("agent server shutting down")
}

func serveStdio(cfg Config, llmProvider LLMConfigProvider, modelInfo ModelInfoProvider, entClient *ent.Client, logger *slog.Logger) {
	ag := NewAgent(cfg, llmProvider, modelInfo)
	conn := acp.NewAgentSideConnection(ag, os.Stdout, os.Stdin)
	conn.SetLogger(logger)
	ag.SetConnection(conn)

	slog.Info("agent server started (stdio)", "version", acp.ProtocolVersionNumber)
	<-conn.Done()
	ag.OnDisconnect(context.Background())
	slog.Info("agent server shutting down")
}

func serveTCP(ctx context.Context, addr string, cfg Config, llmProvider LLMConfigProvider, modelInfo ModelInfoProvider, entClient *ent.Client, logger *slog.Logger) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("failed to listen", "addr", addr, "error", err)
		os.Exit(1)
	}
	defer ln.Close()

	slog.Info("agent server listening (tcp)", "addr", addr, "version", acp.ProtocolVersionNumber)

	var wg sync.WaitGroup
	for {
		raw, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				slog.Info("shutting down, waiting for active connections...")
				wg.Wait()
				return
			default:
				slog.Error("accept error", "error", err)
				continue
			}
		}

		wg.Add(1)
		slog.Info("client connected", "remote", raw.RemoteAddr())
		go func(c net.Conn) {
			defer wg.Done()
			ag := NewAgent(cfg, llmProvider, modelInfo)
			conn := acp.NewAgentSideConnection(ag, c, c)
			conn.SetLogger(logger)
			ag.SetConnection(conn)
			<-conn.Done()
			ag.OnDisconnect(context.Background())
		}(raw)
	}
}

func serveUnix(ctx context.Context, path string, cfg Config, llmProvider LLMConfigProvider, modelInfo ModelInfoProvider, entClient *ent.Client, logger *slog.Logger) {
	_ = os.Remove(path)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Error("failed to create socket dir", "dir", dir, "error", err)
		os.Exit(1)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		slog.Error("failed to listen", "path", path, "error", err)
		os.Exit(1)
	}
	defer ln.Close()
	defer os.Remove(path)

	slog.Info("agent server listening (unix)", "path", path, "version", acp.ProtocolVersionNumber)

	var wg sync.WaitGroup
	for {
		raw, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				slog.Info("shutting down, waiting for active connections...")
				wg.Wait()
				return
			default:
				slog.Error("accept error", "error", err)
				continue
			}
		}

		wg.Add(1)
		slog.Info("client connected", "path", path)
		go func(c net.Conn) {
			defer wg.Done()
			ag := NewAgent(cfg, llmProvider, modelInfo)
			conn := acp.NewAgentSideConnection(ag, c, c)
			conn.SetLogger(logger)
			ag.SetConnection(conn)
			<-conn.Done()
			ag.OnDisconnect(context.Background())
		}(raw)
	}
}
