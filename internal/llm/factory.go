package llm

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino-ext/components/model/openai"

	"acp/internal/config"
)

// NewChatModel creates a ToolCallingChatModel based on the configuration.
func NewChatModel(ctx context.Context, cfg config.Config) (model.ToolCallingChatModel, error) {
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
