package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// SessionMessage stores a single message within a session.
type SessionMessage struct {
	ent.Schema
}

func (SessionMessage) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").
			Unique().
			Immutable().
			Comment("Auto-increment message ID"),

		field.String("session_id").
			Comment("Foreign key to Session"),

		field.Int("seq").
			Comment("Message sequence number within the session"),

		field.Enum("role").
			Values("system", "user", "assistant", "tool").
			Comment("Message role"),

		field.Text("content").
			Optional().
			Default("").
			Comment("Message text content"),

		field.JSON("tool_calls", []ToolCall{}).
			Optional().
			Default(func() []ToolCall { return []ToolCall{} }).
			Comment("Assistant tool calls (only for role=assistant)"),

		field.String("tool_call_id").
			Optional().
			Default("").
			Comment("Tool call ID for matching tool result to call (only for role=tool)"),

		field.Time("create_time").
			Immutable().
			Default(time.Now).
			Comment("Message creation timestamp"),
	}
}

func (SessionMessage) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("session", Session.Type).
			Ref("messages").
			Unique().
			Required().
			Field("session_id"),
	}
}

func (SessionMessage) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("session_id", "seq").
			Unique(),
		index.Fields("session_id"),
	}
}

// ToolCall represents a single tool call within an assistant message.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Result    string         `json:"result,omitempty"`
}
