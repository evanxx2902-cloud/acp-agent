package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Session holds the persistent state of an ACP session.
type Session struct {
	ent.Schema
}

func (Session) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			Unique().
			Immutable().
			Comment("Session unique ID (UUID v4, server-generated)"),

		field.Enum("status").
			Values("active", "idle", "closed").
			Default("active").
			Comment("Session status: active | idle | closed"),

		field.Int64("user_id").
			Optional().
			Default(0).
			Comment("Authenticated user ID"),

		field.String("username").
			Optional().
			Default("").
			Comment("Authenticated username"),

		field.String("business_id").
			Optional().
			Default("").
			Comment("Business context identifier"),

		field.String("business_type").
			Optional().
			Default("").
			Comment("Business context type (e.g., project, workspace)"),

		field.JSON("business_meta", map[string]any{}).
			Optional().
			Default(func() map[string]any { return map[string]any{} }).
			Comment("Extensible business metadata"),

		field.String("mode").
			Default("agent").
			Immutable().
			Comment("Execution mode: agent | plan. Immutable after creation."),

		field.Text("summary").
			Optional().
			Default("").
			Comment("Conversation summary, updated by summarization middleware"),

		field.Time("create_time").
			Immutable().
			Default(time.Now).
			Comment("Session creation timestamp"),

		field.Time("update_time").
			Default(time.Now).
			UpdateDefault(time.Now).
			Comment("Session last update timestamp"),
	}
}

func (Session) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("messages", SessionMessage.Type),
	}
}

func (Session) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status"),
		index.Fields("user_id"),
		index.Fields("business_id", "business_type"),
	}
}
