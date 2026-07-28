package main

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino-ext/components/model/openai"

)

// NewChatModel creates a ToolCallingChatModel based on the configuration.
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
