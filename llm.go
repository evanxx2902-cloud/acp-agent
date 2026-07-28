package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino-ext/components/model/openai"
)

// Config holds all configuration for the agent server.
type Config struct {
	LLMProvider   string `json:"llm_provider"`
	LLMAPIKey     string `json:"llm_api_key"`
	LLMBaseURL    string `json:"llm_base_url"`
	LLMModel      string `json:"llm_model"`
	SystemPrompt  string `json:"system_prompt"`
	MaxIterations int    `json:"max_iterations"`
	DataDir       string `json:"data_dir"`
	DBPath        string `json:"db_path"`
}

func DefaultConfig() Config {
	return Config{
		LLMProvider:   "openai-compatible",
		LLMModel:      "gpt-4o",
		MaxIterations: 20,
		SystemPrompt:  "You are a helpful AI assistant. You can read and write files on the user's system using the available tools.",
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
	if v := os.Getenv("ACP_SYSTEM_PROMPT"); v != "" {
		cfg.SystemPrompt = v
	}
	if v := os.Getenv("ACP_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("ACP_DB_PATH"); v != "" {
		cfg.DBPath = v
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
