package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"strconv"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino-ext/components/model/openai"
)

// Config holds all configuration for the agent server.
// Only LLM-related fields are loaded from config.json.
// Operational fields use code defaults + env var overrides.
type Config struct {
	// LLM config (from config.json + env vars)
	LLMProvider   string `json:"llm_provider"`
	LLMAPIKey     string `json:"llm_api_key"`
	LLMBaseURL    string `json:"llm_base_url"`
	LLMModel      string `json:"llm_model"`
	ContextWindow int    `json:"context_window"`

	// Operational config (code defaults + env vars only, NOT in config.json)
	SummarizationTrigger float64 // fraction of context window to trigger summarization (0.0-1.0)
	SystemPrompt         string
	MaxIterations        int
	DataDir              string
	DBPath               string
	Listen               string
	LogLevel             string
}

func DefaultConfig() Config {
	return Config{
		LLMProvider:          "openai-compatible",
		LLMModel:             "gpt-4o",
		ContextWindow:        131072,
		SummarizationTrigger: 0.5,
		SystemPrompt:         "You are a helpful AI assistant.",
		MaxIterations:        20,
		Listen:               "stdio",
		LogLevel:             "info",
	}
}

func LoadConfig() Config {
	cfg := DefaultConfig()

	configPath := flag.String("config", "", "Path to JSON config file")
	flag.Parse()

	if *configPath != "" {
		data, err := os.ReadFile(*configPath)
		if err == nil {
			_ = json.Unmarshal(data, &cfg)
		}
	}

	// LLM env vars
	if v := os.Getenv("ACP_LLM_PROVIDER"); v != "" {
		cfg.LLMProvider = v
	}
	if v := os.Getenv("ACP_LLM_API_KEY"); v != "" {
		cfg.LLMAPIKey = v
	}
	if v := os.Getenv("ACP_LLM_BASE_URL"); v != "" {
		cfg.LLMBaseURL = v
	}
	if v := os.Getenv("ACP_LLM_MODEL"); v != "" {
		cfg.LLMModel = v
	}
	if v := os.Getenv("ACP_CONTEXT_WINDOW"); v != "" {
		if n, _ := strconv.Atoi(v); n > 0 {
			cfg.ContextWindow = n
		}
	}

	// Operational env vars
	if v := os.Getenv("ACP_SYSTEM_PROMPT"); v != "" {
		cfg.SystemPrompt = v
	}
	if v := os.Getenv("ACP_SUMMARIZATION_TRIGGER"); v != "" {
		if f, _ := strconv.ParseFloat(v, 64); f > 0 && f <= 1.0 {
			cfg.SummarizationTrigger = f
		}
	}
	if v := os.Getenv("ACP_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("ACP_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("ACP_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("ACP_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}

	if cfg.DataDir == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			cfg.DataDir = home + "/.acp-agent"
		} else {
			cfg.DataDir = ".acp-agent"
		}
	}
	if cfg.DBPath == "" {
		cfg.DBPath = cfg.DataDir + "/sessions.db"
	}

	return cfg
}

func NewChatModel(ctx context.Context, cfg Config) (model.ToolCallingChatModel, error) {
	baseURL := cfg.LLMBaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey:  cfg.LLMAPIKey,
		BaseURL: baseURL,
		Model:   cfg.LLMModel,
	})
}
