package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino-ext/components/model/openai"
)

// =========================================================================
// LLMConfigProvider / ModelInfoProvider interfaces
// =========================================================================

type LLMConfig struct {
	Provider string
	APIKey   string
	BaseURL  string
	Model    string
}

type LLMConfigProvider interface {
	GetConfig(ctx context.Context) (*LLMConfig, error)
}

type ModelInfoProvider interface {
	GetContextWindow(ctx context.Context, model string) (int, error)
}

// =========================================================================
// Default implementation: reads from config file + env vars
// =========================================================================

type DefaultLLMConfigProvider struct {
	configPath string
}

func NewDefaultLLMConfigProvider(configPath string) *DefaultLLMConfigProvider {
	return &DefaultLLMConfigProvider{configPath: configPath}
}

func (p *DefaultLLMConfigProvider) GetConfig(ctx context.Context) (*LLMConfig, error) {
	cfg := &LLMConfig{
		Provider: "openai-compatible",
		Model:    "gpt-4o",
	}

	// Try loading from JSON config file
	if p.configPath != "" {
		if data, err := os.ReadFile(p.configPath); err == nil {
			var fileCfg struct {
				LLMProvider string `json:"llm_provider"`
				LLMAPIKey   string `json:"llm_api_key"`
				LLMBaseURL  string `json:"llm_base_url"`
				LLMModel    string `json:"llm_model"`
			}
			if json.Unmarshal(data, &fileCfg) == nil {
				if fileCfg.LLMProvider != "" {
					cfg.Provider = fileCfg.LLMProvider
				}
				if fileCfg.LLMAPIKey != "" {
					cfg.APIKey = fileCfg.LLMAPIKey
				}
				if fileCfg.LLMBaseURL != "" {
					cfg.BaseURL = fileCfg.LLMBaseURL
				}
				if fileCfg.LLMModel != "" {
					cfg.Model = fileCfg.LLMModel
				}
			}
		}
	}

	// Env var overrides
	if v := os.Getenv("ACP_LLM_PROVIDER"); v != "" {
		cfg.Provider = v
	}
	if v := os.Getenv("ACP_LLM_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("ACP_LLM_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := os.Getenv("ACP_LLM_MODEL"); v != "" {
		cfg.Model = v
	}

	return cfg, nil
}

// =========================================================================
// Default ModelInfoProvider
// =========================================================================

type DefaultModelInfoProvider struct{}

func (p *DefaultModelInfoProvider) GetContextWindow(ctx context.Context, model string) (int, error) {
	// Default context window, can be overridden by env var
	if v := os.Getenv("ACP_CONTEXT_WINDOW"); v != "" {
		if n, _ := strconv.Atoi(v); n > 0 {
			return n, nil
		}
	}
	// Known model defaults
	switch model {
	case "gpt-4o":
		return 131072, nil
	case "gpt-4-turbo":
		return 131072, nil
	case "gpt-4":
		return 8192, nil
	case "gpt-3.5-turbo":
		return 16385, nil
	case "deepseek-chat":
		return 131072, nil
	case "claude-3-opus":
		return 200000, nil
	case "claude-3-sonnet":
		return 200000, nil
	default:
		return 131072, nil
	}
}

// =========================================================================
// NewChatModel creates a chat model from LLM config
// =========================================================================

func NewChatModel(ctx context.Context, cfg *LLMConfig) (model.ToolCallingChatModel, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey:  cfg.APIKey,
		BaseURL: baseURL,
		Model:   cfg.Model,
	})
}
