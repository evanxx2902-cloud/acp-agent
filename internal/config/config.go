package config

import (
	"encoding/json"
	"flag"
	"os"
)

// Config holds all configuration for the agent server.
type Config struct {
	// LLM backend configuration
	LLMProvider   string `json:"llm_provider"`   // "openai" or "openai-compatible"
	LLMAPIKey     string `json:"llm_api_key"`
	LLMBaseURL    string `json:"llm_base_url"`    // optional custom base URL
	LLMModel      string `json:"llm_model"`        // e.g. "gpt-4o", "deepseek-chat"

	// Agent configuration
	SystemPrompt  string `json:"system_prompt"`
	MaxIterations int    `json:"max_iterations"` // default 20
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		LLMProvider:   "openai-compatible",
		LLMModel:      "gpt-4o",
		MaxIterations: 20,
		SystemPrompt:  "You are a helpful AI assistant. You can read and write files on the user's system using the available tools.",
	}
}

// Load reads configuration from environment variables and an optional config file.
func Load() Config {
	cfg := DefaultConfig()

	// Optional config file
	configPath := flag.String("config", "", "Path to JSON config file")
	flag.Parse()

	if *configPath != "" {
		data, err := os.ReadFile(*configPath)
		if err == nil {
			_ = json.Unmarshal(data, &cfg)
		}
	}

	// Environment variable overrides
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

	return cfg
}
