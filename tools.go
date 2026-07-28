package main

import (
	"context"

	"github.com/coder/acp-go-sdk"
)

// --------------------------------------------------------------------------
// ACP context helpers
// --------------------------------------------------------------------------

type acpContextKey struct{}

type acpContext struct {
	Conn      *acp.AgentSideConnection
	SessionID acp.SessionId
}

// ContextWithACP stores the ACP connection and session ID in the context.
func ContextWithACP(ctx context.Context, conn *acp.AgentSideConnection, sessionID acp.SessionId) context.Context {
	return context.WithValue(ctx, acpContextKey{}, &acpContext{
		Conn:      conn,
		SessionID: sessionID,
	})
}

func getACPContext(ctx context.Context) (*acp.AgentSideConnection, acp.SessionId) {
	if v, ok := ctx.Value(acpContextKey{}).(*acpContext); ok {
		return v.Conn, v.SessionID
	}
	return nil, ""
}
