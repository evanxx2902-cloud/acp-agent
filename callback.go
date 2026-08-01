package main

import (
	"context"
	"log/slog"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"
)

// =========================================================================
// agentCallback — observability hooks for eino component lifecycle
// =========================================================================

type agentCallback struct {
	callbacks.Handler
}

func newAgentCallback() callbacks.Handler {
	return &agentCallback{}
}

func (c *agentCallback) OnStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
	slog.Debug("callback: start", "component", info.Component, "name", info.Name, "type", info.Type)
	return ctx
}

func (c *agentCallback) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	slog.Debug("callback: end", "component", info.Component, "name", info.Name, "type", info.Type)
	return ctx
}

func (c *agentCallback) OnError(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
	slog.Warn("callback: error", "component", info.Component, "name", info.Name, "type", info.Type, "error", err)
	return ctx
}

func (c *agentCallback) OnStartWithStreamInput(ctx context.Context, info *callbacks.RunInfo, input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
	return ctx
}

func (c *agentCallback) OnEndWithStreamOutput(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
	return ctx
}

func (c *agentCallback) Needed(_ context.Context, _ *callbacks.RunInfo, timing callbacks.CallbackTiming) bool {
	return timing == callbacks.TimingOnStart || timing == callbacks.TimingOnEnd || timing == callbacks.TimingOnError
}
