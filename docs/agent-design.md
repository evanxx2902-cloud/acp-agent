# Agent Server 技术方案文档

## 1. 概述

Agent Server 是一个基于 ACP（Agent-Client Protocol）协议的多会话 AI Agent 服务端。它接收客户端连接，管理会话生命周期，编排 LLM + MCP 工具的 ReAct 推理循环，并将结果流式返回给客户端。

### 1.1 技术栈

| 组件 | 选型 | 说明 |
|------|------|------|
| 框架 | [Kratos](https://go-kratos.dev/) | HTTP/gRPC 服务框架，提供配置、日志、中间件、依赖注入 |
| ORM | [Ent](https://entgo.io/) | 类型安全的 Go ORM，自动生成数据模型代码 |
| 存储 | SQLite（开发）/ PostgreSQL（生产） | 通过 Ent 的数据库驱动适配 |
| AI 框架 | [Eino](https://github.com/cloudwego/eino) | 字节跳动开源的 AI Agent 框架，提供 ChatModelAgent、ReAct、Runner |
| LLM | OpenAI 兼容 API | 通过 eino-ext 的 OpenAI adapter |
| MCP | [mcp-go](https://github.com/mark3labs/mcp-go) | Model Context Protocol 客户端库 |
| 协议 | ACP (JSON-RPC 2.0) | 通过 acp-go-sdk |

### 1.2 工程结构

```
acp/
├── cmd/
│   └── agent-server/
│       └── main.go              # 启动入口
├── internal/
│   ├── server/
│   │   ├── server.go            # Kratos server 初始化和 Wire 注入
│   │   └── acp_handler.go       # ACP 协议 JSON-RPC 处理器
│   ├── agent/
│   │   ├── agent.go             # Agent 核心，实现 acp.Agent 接口
│   │   ├── runner.go            # ReAct 推理循环
│   │   └── callback.go          # Eino 回调（日志、telemetry）
│   ├── session/
│   │   ├── manager.go           # SessionManager — 内存缓存 + 生命周期
│   │   ├── scanner.go           # IdleScanner — 心跳超时检测
│   │   └── session.go           # 运行时 Session 对象
│   ├── mcp/
│   │   ├── manager.go           # MCP 连接管理器
│   │   └── tool_adapter.go      # MCP Tool → Eino BaseTool 适配器
│   ├── llm/
│   │   ├── config.go            # LLM 配置
│   │   └── factory.go           # ChatModel 工厂
│   └── middleware/
│       ├── summarization.go     # 对话摘要中间件
│       └── plantask.go          # Plan 模式中间件
├── ent/
│   ├── schema/
│   │   ├── session.go           # Session 实体定义
│   │   └── session_message.go   # SessionMessage 实体定义
│   ├── client.go                # 生成的 Ent Client
│   └── ...                      # 其他生成文件
├── configs/
│   └── config.yaml              # Kratos 配置文件
├── go.mod
└── go.sum
```

---

## 2. 数据模型设计（Ent Schema）

### 2.1 Session 实体

```go
// ent/schema/session.go
package schema

import (
    "time"
    "entgo.io/ent"
    "entgo.io/ent/schema/field"
    "entgo.io/ent/schema/index"
)

// Session holds the session entity definition.
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
            Comment("Authenticated user ID"),

        field.String("username").
            Optional().
            Comment("Authenticated username"),

        field.String("business_id").
            Optional().
            Comment("Business context identifier"),

        field.String("business_type").
            Optional().
            Comment("Business context type (e.g., project, workspace)"),

        field.JSON("business_meta", map[string]any{}).
            Optional().
            Default(func() map[string]any { return map[string]any{} }).
            Comment("Extensible business metadata"),

        field.String("mode").
            Default("agent").
            Immutable().
            Comment("Execution mode: agent | plan. Immutable after creation."),

        field.Int("heartbeat_interval").
            Default(10).
            Comment("Client heartbeat interval in seconds, server timeout = 3x this value"),

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

func (Session) Indexes() []ent.Index {
    return []ent.Index{
        index.Fields("status"),
        index.Fields("user_id"),
        index.Fields("business_id", "business_type"),
    }
}
```

### 2.2 SessionMessage 实体

```go
// ent/schema/session_message.go
package schema

import (
    "time"
    "entgo.io/ent"
    "entgo.io/ent/schema/field"
    "entgo.io/ent/schema/edge"
    "entgo.io/ent/schema/index"
)

// SessionMessage holds a single message in a session conversation.
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
    Result    string         `json:"result,omitempty"` // lightweight summary
}
```

### 2.3 ER 关系图

```
┌──────────────────────────────────────┐
│              Session                 │
├──────────────────────────────────────┤
│ id            TEXT PK (UUID v4)      │
│ status        TEXT (active|idle|closed)│
│ user_id       INTEGER                │
│ username      TEXT                   │
│ business_id   TEXT                   │
│ business_type TEXT                   │
│ business_meta JSON                   │
│ mode          TEXT (agent|plan)      │
│ heartbeat_interval INTEGER           │
│ summary       TEXT                   │
│ create_time   TIMESTAMP              │
│ update_time   TIMESTAMP              │
└──────────┬───────────────────────────┘
           │ 1
           │
           │ *
┌──────────▼───────────────────────────┐
│          SessionMessage              │
├──────────────────────────────────────┤
│ id           INTEGER PK (auto-incr)  │
│ session_id   TEXT FK → Session       │
│ seq          INTEGER                 │
│ role         TEXT (sys|user|asst|tool)│
│ content      TEXT                    │
│ tool_calls   JSON                    │
│ tool_call_id TEXT                    │
│ create_time  TIMESTAMP               │
└──────────────────────────────────────┘
```

### 2.4 数据归属

**Session 表持久化的字段：**

| 字段 | 说明 |
|------|------|
| `id`, `status`, `mode` | 会话核心标识和状态 |
| `user_id`, `username` | 会话归属 |
| `business_id`, `business_type`, `business_meta` | 业务上下文 |
| `heartbeat_interval` | 客户端声明的心跳间隔，服务端据此计算超时（3x） |
| `summary` | 对话摘要 |

**每次请求由客户端携带的数据：**

| 数据 | 携带接口 |
|------|----------|
| `mcp_servers` | session/new, session/resume |

> `system_prompt` 不存 session 表——`session/new` 时作为 `session_messages` 第一条记录（seq=0, role=system）持久化。





ACP 协议基于 JSON-RPC 2.0，Client 发起请求，Agent Server 响应。以下是 Agent Server 需要实现的全部接口。

### 3.1 接口总览

| 接口 | 方向 | 职责 |
|------|------|------|
| `Initialize` | Client → Server | 协商协议版本和能力集 |
| `Authenticate` | Client → Server | 客户端认证 |
| `session/new` | Client → Server | 创建新会话，返回 session_id |
| `session/prompt` | Client → Server | 向会话发送用户消息并启动推理 |
| `session/resume` | Client → Server | 从 DB 加载 idle 会话并恢复为 active |
| `session/close` | Client → Server | 关闭会话 |
| `session/list` | Client → Server | 列出用户的所有会话 |
| `session/cancel` | Client → Server | 取消当前推理 |
| `_heartbeat` | Client → Server | 心跳扩展，保持会话 active |
| `_release` | Client → Server | 释放扩展，主动标记会话为 idle |
| `SessionUpdate` | Server → Client | 通知——流式输出（增量文本、思考、工具调用） |
| `RequestPermission` | Server → Client | 请求——要求用户授权工具执行 |

### 3.2 接口详解

#### 3.2.1 Initialize

协商协议版本和能力集，客户端连接后第一个请求。

**请求参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `protocol_version` | string | 是 | 客户端支持的 ACP 协议版本 |
| `client_info` | object | 是 | 客户端名称、版本等标识信息 |
| `capabilities` | object | 否 | 客户端支持的能力集 |

**响应字段：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `protocol_version` | string | 协商后的协议版本 |
| `server_info` | object | 服务端名称、版本 |
| `capabilities` | object | 服务端支持的能力集（streaming, mcp, plan, heartbeat） |

**职责：**
- 协商 ACP 协议版本
- 声明 Server 支持的能力集
- 初始化服务器端资源

---

#### 3.2.2 Authenticate

验证客户端凭据，建立认证上下文。

**请求参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `credential` | string | 是 | 认证凭据（token / API key） |

**响应：** 空对象 `{}`，认证成功。失败返回 JSON-RPC error。

**职责：**
- 验证客户端凭据
- 建立认证上下文，后续请求均在认证上下文中执行

---

#### 3.2.3 session/new

创建新会话，连接 MCP Server，构建 Agent。不立即执行推理。

**请求参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `mcp_servers` | MCPConfig[] | 是 | MCP 服务器连接配置列表 |
| `mode` | string | 否 | 执行模式：`"agent"`（默认）或 `"plan"`，创建后不可变 |
| `system_prompt` | string | 否 | 系统提示词，持久化为 session_messages 第一条记录（seq=0, role=system），创建后不可变 |
| `max_iterations` | int | 否 | 单次推理最大轮次，默认 `20`，不可超过服务端硬上限 |
| `heartbeat_interval` | int | 否 | 客户端心跳间隔（秒），默认 10。服务端超时时间 = 3 × heartbeat_interval |
| `business_id` | string | 否 | 业务上下文标识 |
| `business_type` | string | 否 | 业务上下文类型 |
| `business_meta` | object | 否 | 扩展业务元数据 |

**响应字段：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `session_id` | string | 服务端生成的 UUID v4，后续所有操作的主键 |
| `status` | string | `"active"` |

**职责：**
- 服务端生成 `session_id`
- 创建 Session 数据库记录，`system_prompt` 作为 session_messages 的第一条消息（seq=0）持久化
- 连接客户端声明的 MCP Server，发现并注册工具
- 构建 Eino ChatModelAgent 实例
- 将 Session 加入内存缓存

---

#### 3.2.4 session/prompt

向会话发送用户消息并启动 ReAct 推理循环。仅 `active` 状态可用。

**请求参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | 是 | 会话 ID |
| `messages` | Message[] | 是 | 用户消息列表，每条含 `role` 和 `content` |

> `system_prompt` 在 `session/new` 时已固定为第一条消息，prompt 时不可覆盖。

**响应：**

流式 `SessionUpdate` 通知序列，最后返回完成信号：

| 字段 | 类型 | 说明 |
|------|------|------|
| (流式) | SessionUpdate | `AgentMessageChunk`、`AgentThoughtChunk`、`StartToolCall`、`UpdateToolCall` |
| (最终) `status` | string | `"completed"` / `"cancelled"` / `"error"` |

**职责：**
- 验证 Session 必须处于 `active` 状态
- 将用户消息追加到 session_messages，追加到 Eino 消息列表
- 启动 ReAct 推理循环，最多执行 `max_iterations` 轮
- 工具执行前通过 `RequestPermission` 请求用户授权
- 推理结束后保存完整消息历史到数据库

---

#### 3.2.5 session/resume

从 DB 加载 idle 会话，恢复 MCP 连接和 ChatModelAgent。仅 `idle` 状态可用。

**请求参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | 是 | 会话 ID |
| `mcp_servers` | MCPConfig[] | 是 | MCP 服务器连接配置（客户端重新传入） |
| `prompt` | string | 否 | 可选的 continuation prompt，不传则仅恢复状态不推理 |

**响应：** 同 `session/prompt`（如果传了 prompt 则启动推理流式输出，否则返回成功确认）。

**职责：**
- 验证 Session 必须处于 `idle` 状态
- 从 session 表加载元数据（mode、summary、business_meta等），从 session_messages 表加载完整历史（第一条 role=system 的消息即为 system_prompt）
- 使用客户端重新传入的 `mcp_servers` 重建 MCP 连接和 ChatModelAgent
- 将状态从 `idle` 恢复为 `active`
- 如果传了 prompt，启动 ReAct 推理循环

> 不需要单独的 load 接口——resume 就是「加载 + 恢复」。客户端先 `session/list` 浏览会话，再 `session/resume` 进入对话。

---

#### 3.2.6 session/close

关闭会话，释放资源。不可逆操作。

**请求参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | 是 | 会话 ID |

**响应字段：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `session_id` | string | 会话 ID |
| `status` | string | `"closed"` |

**职责：**
- 验证 Session 必须处于 `active` 或 `idle` 状态
- 断开所有 MCP 连接
- 将数据库中的状态更新为 `closed`
- 从内存缓存中移除

---

#### 3.2.7 session/list

查询用户可见的会话列表，支持过滤和分页。

**请求参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `user_id` | int64 | 否 | 按用户过滤 |
| `business_id` | string | 否 | 按业务标识过滤 |
| `business_type` | string | 否 | 按业务类型过滤 |
| `business_meta` | object | 否 | 按 business_meta 中的 key-value 过滤，如 `{"project": "acp"}` |
| `status` | string | 否 | 按状态过滤 |
| `page_size` | int | 否 | 每页条数，默认 20 |
| `page_number` | int | 否 | 页码，从 1 开始，默认 1 |

**响应字段：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `sessions` | SessionMeta[] | 会话摘要列表 |
| `total` | int | 符合条件的记录总数 |

每条 `SessionMeta`：

| 字段 | 类型 | 说明 |
|------|------|------|
| `session_id` | string | 会话 ID |
| `status` | string | active / idle / closed |
| `mode` | string | agent / plan |
| `user_id` | int64 | 所属用户 ID |
| `username` | string | 所属用户名 |
| `business_id` | string | 业务标识 |
| `business_type` | string | 业务类型 |
| `business_meta` | object | 业务元数据 |
| `message_count` | int | 消息总数（不含 system prompt） |
| `create_time` | string | 创建时间 |
| `update_time` | string | 最后更新时间 |

**职责：**
- 支持多条件 AND 过滤；`business_meta` 过滤使用 JSON 字段匹配（SQLite: `json_extract`, PostgreSQL: `->>`）
- 分页查询，按 `update_time DESC` 排序
- 返回 `total` 供前端计算总页数
- 不返回消息详情

---

#### 3.2.8 session/cancel

取消当前正在执行的推理循环。

**请求参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | 是 | 会话 ID |

**响应：** 空对象 `{}`。

**职责：**
- 调用 context.CancelFunc，使 ReAct 循环在下一个检查点退出
- 已生成的中间结果保留在消息历史中
- Session 状态保持 `active`

---

#### 3.2.9 _heartbeat（扩展方法）

客户端定期发送心跳，防止 Session 被自动标记为 idle。

**请求参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | 是 | 会话 ID |

**响应：** 空对象 `{}`。

**职责：**
- 仅对 `active` 状态的 Session 生效
- 更新内存中的 `lastHeartbeat` 时间戳
- 客户端建议每 3 秒发送一次

---

#### 3.2.10 _release（扩展方法）

客户端主动释放 Session 控制权，标记为 idle。

**请求参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | 是 | 会话 ID |

**响应：** 空对象 `{}`。

**职责：**
- 如果 Session 是 `active`，标记为 `idle` 并更新 DB
- 断开所有 MCP 连接（idle 状态的 Session 不保留 MCP 连接）
- 保留 RuntimeSession 在内存中（含消息历史和 summary），MCPManager 置空
- 如果已经是 `closed`，忽略

---

## 4. Session 状态机

### 4.1 状态定义

| 状态 | 含义 | 允许的操作 |
|------|------|------------|
| `active` | 会话活跃，正在进行推理或等待用户输入 | prompt, cancel, close, heartbeat, release |
| `idle` | 会话空闲，MCP 连接已释放，消息历史和 summary 保留在内存 | resume, close |
| `closed` | 会话已关闭，不可逆终态 | 无 |

### 4.2 状态流转图

```
                    ┌──────────────┐
                    │              │
     session/new    │    ACTIVE    │◄────────┐
        ──────────► │              │          │
                    └───┬────┬────┘          │
                        │    │               │
          session/close │    │ _release      │ session/resume
                        │    │ (heartbeat    │
                        │    │  timeout)     │
                        │    │               │
                        ▼    ▼               │
                    ┌──────────┐             │
                    │          │             │
                    │  CLOSED  │    IDLE     │
                    │          │─────┼───────┘
                    └──────────┘     │
                          ▲          │ session/close
                          │          │
                          └──────────┘
                          (可以从 idle 直接 close)
```

### 4.3 状态转换表

| 当前状态 | 触发事件 | 目标状态 | 说明 |
|----------|----------|----------|------|
| - | `session/new` | `active` | 新建会话，立即激活 |
| `active` | `session/prompt` | `active` | 保持活跃，执行推理 |
| `active` | `session/cancel` | `active` | 取消推理，仍可继续对话 |
| `active` | `_release` | `idle` | 客户端主动释放控制权 |
| `active` | heartbeat 超时 | `idle` | IdleScanner 检测到 lastHeartbeat 超过 3×heartbeat_interval |
| `active` | `session/close` | `closed` | 主动关闭 |
| `idle` | `session/resume` | `active` | 从 DB 加载会话，恢复 MCP 连接，继续对话 |
| `idle` | `session/close` | `closed` | 从空闲状态直接关闭 |
| `closed` | 任何操作 | `closed` | 终态，拒绝所有操作 |

### 4.4 IdleScanner 机制

后台 goroutine 每隔 10 秒扫描所有 `active` Session，检查 `now - lastHeartbeat > 3 × heartbeat_interval`，超过则标记为 idle，断开 MCP 连接，更新 DB 状态。

扫描时跳过 `idle` 和 `closed` 状态的 Session。

---

## 5. 核心结构体定义与关系

### 5.1 结构体关系全景图

```
┌────────────────────────────────────────────────────────────────────┐
│                         Kratos App                                  │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │                      Wire (DI)                                │  │
│  │   Config → EntClient → SessionRepo → SessionManager          │  │
│  │   Config → LLM Factory → ChatModel                           │  │
│  │   SessionManager → Agent → ACPHandler → Kratos Server        │  │
│  └──────────────────────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────┐
│                          Agent                                    │
│  (实现 acp.Agent + acp.AgentLoader 接口)                           │
├──────────────────────────────────────────────────────────────────┤
│  cfg       Config                                                 │
│  chatModel ToolCallingChatModel  ◄── LLM Factory 创建             │
│  sessions  *SessionManager       ◄── 内存缓存 + 生命周期           │
│  conn      *AgentSideConnection  ◄── ACP 协议连接                  │
└───────────────┬──────────────────────────────────────────────────┘
                │ 管理
                ▼
┌──────────────────────────────────────────────────────────────────┐
│                      SessionManager                               │
├──────────────────────────────────────────────────────────────────┤
│  mu        sync.Mutex                                             │
│  sessions  map[string]*RuntimeSession   ◄── 内存中的活跃会话       │
│  sessionRepo   *ent.SessionClient       ◄── 持久化                 │
│  messageRepo   *ent.SessionMessageClient◄── 持久化                 │
│  idleScanner   *IdleScanner                                       │
├──────────────────────────────────────────────────────────────────┤
│  + NewSession(ctx, params) → *RuntimeSession, sessionID           │
│  + GetCached(id) → *RuntimeSession, bool                           │
│  + Resume(ctx, id, mcpServers) → *RuntimeSession                  │
│  + Close(ctx, id)                                                  │
│  + MarkIdle(ctx, id)                                               │
│  + List(ctx, filter) → []SessionMeta                              │
└───────────────┬──────────────────────────────────────────────────┘
                │ 包含
                ▼
┌──────────────────────────────────────────────────────────────────┐
│                     RuntimeSession                                │
├──────────────────────────────────────────────────────────────────┤
│  ID            string                                             │
│  mu            sync.Mutex                                         │
│  Status        SessionStatus  (active | idle | closed)            │
│  Mode              string         (agent | plan)                      │
│  Summary           string         ◄── 对话摘要（摘要中间件更新）        │
│  HeartbeatInterval int            ◄── 客户端心跳间隔（秒）              │
│  Messages          []*schema.Message  ◄── Eino 消息格式               │
│  Seq               int                                                │
│  Cancel            context.CancelFunc                                 │
│  Ctx               context.Context                                    │
│  LastHeartbeat     time.Time                                          │
│  MaxIterations int           ◄── 单次推理最大轮次                  │
│  MCPManager    *MCPManager      ◄── MCP 连接池                    │
│  CMAgent       *ChatModelAgent  ◄── Eino Agent 实例                │
│  BusinessMeta  BusinessMeta     ◄── 透传业务元数据                  │
├──────────────────────────────────────────────────────────────────┤
│  + AppendMessage(msg)                                              │
│  + AppendToolResult(toolCallID, result)                            │
│  + SaveMessages(ctx)                                               │
│  + UpdateSummary(ctx, summary)                                     │
│  + BuildAgent(chatModel, mcpTools, mode)                           │
│  + IsActive() bool                                                 │
│  + IsClosed() bool                                                 │
│  + TransitionStatus(newStatus) error                               │
└───────────────┬──────────────────────────────────────────────────┘
                │ 持有
                ▼
┌──────────────────────────────────────────────────────────────────┐
│                      MCPManager                                   │
├──────────────────────────────────────────────────────────────────┤
│  Clients  []*MCPClientWrapper    ◄── MCP 客户端列表                │
│  Tools    []*ToolAdapter         ◄── 适配后的工具列表               │
├──────────────────────────────────────────────────────────────────┤
│  + Connect(ctx, servers []MCPConfig) error                        │
│  + DiscoverTools(ctx) → []*schema.ToolInfo                        │
│  + Disconnect()                                                    │
│  + GetTools() → []tool.BaseTool                                   │
└───────────────┬──────────────────────────────────────────────────┘
                │ 包含
                ▼
┌──────────────────────────────────────────────────────────────────┐
│                      ToolAdapter                                  │
│  (实现 eino tool.InvokableTool 接口)                                │
├──────────────────────────────────────────────────────────────────┤
│  Info   *schema.ToolInfo                                          │
│  Client MCPCaller                                                 │
├──────────────────────────────────────────────────────────────────┤
│  + Info() → *schema.ToolInfo                                      │
│  + InvokableRun(ctx, args) → result                               │
│    - 发送 StartToolCall 通知                                       │
│    - 请求用户权限 (RequestPermission)                               │
│    - 执行 MCP 调用                                                  │
│    - 发送 UpdateToolCall 结果                                      │
└──────────────────────────────────────────────────────────────────┘
```

### 5.2 关键类型定义

```go
// MCPConfig 描述一个 MCP 服务器的连接方式（客户端在 new/load/resume 时传入）。
// 该结构体不在 Ent Schema 中持久化，由客户端每次请求携带。
type MCPConfig struct {
    Name    string            `json:"name"`
    Command string            `json:"command,omitempty"`   // stdio 模式
    Args    []string          `json:"args,omitempty"`      // stdio 模式
    Env     map[string]string `json:"env,omitempty"`       // stdio 模式
    URL     string            `json:"url,omitempty"`        // sse / streamable_http 模式
    Headers map[string]string `json:"headers,omitempty"`   // sse / streamable_http 模式
    Type    string            `json:"type"`                 // "stdio" | "sse" | "streamable_http"
}
```

### 5.3 Ent 生成的 Repo 层

Ent 通过 schema 定义自动生成类型安全的 Client，我们用它作为 Repository 层：

```go
// EntClient 持有数据库连接和所有生成的子 Client
type EntClient = ent.Client   // 由 ent 生成

// 使用方式：
// client.Session.Query().Where(session.StatusEQ("active")).All(ctx)
// client.SessionMessage.Query().Where(
//     sessionmessage.SessionIDEQ(sid),
//     sessionmessage.SeqGT(lastSeq),
// ).All(ctx)
```

无需额外封装 Repository 接口，Ent 生成的 Client 已经提供了完整的 CRUD 操作、查询构建器、事务支持。

### 5.4 依赖注入（Kratos Wire）

```go
// wire.go
//go:build wireinject
// +build wireinject

func InitApp(cfg *conf.Bootstrap) (*App, error) {
    wire.Build(
        // 数据库
        NewEntClient,
        // LLM
        NewChatModel,
        // MCP
        mcp.NewManager,
        // Session
        session.NewSessionManager,
        session.NewIdleScanner,
        // Agent
        agent.NewAgent,
        // Server
        server.NewACPServer,
        server.NewACPHandler,
        // App
        NewApp,
    )
    return &App{}, nil
}
```

### 5.5 各结构体职责总结

| 结构体 | 职责 |
|--------|------|
| `App` | 应用程序入口，组合 Kratos Server 生命周期 |
| `Agent` | 实现 ACP 协议所有方法，编排 LLM + MCP + Session |
| `SessionManager` | Session 的内存缓存、持久化、List 查询 |
| `RuntimeSession` | 单个会话的运行时状态、消息列表、MCP 连接、Eino Agent |
| `IdleScanner` | 后台检测 heartbeat 超时，自动标记 idle |
| `MCPManager` | MCP 客户端列表管理、工具发现、连接生命周期 |
| `ToolAdapter` | 将 MCP Tool 适配为 Eino InvokableTool，注入权限请求逻辑 |
| `ACPServer` | 管理 ACP Transport（stdio / TCP / Unix Socket） |
| `ACPHandler` | JSON-RPC 请求分发，路由到 Agent 对应方法 |
| `EntClient` | Ent 生成的数据库 Client，作为 Repository 层 |
| `Config` | 全局配置结构体，通过 Kratos config 加载 |
| `ChatModel` | OpenAI 兼容的 LLM 适配器，由 Eino factory 创建 |

### 5.6 接口定义

```go
// SessionManager 的公共接口
type SessionStore interface {
    Create(ctx context.Context, s *SessionMeta) error
    UpdateStatus(ctx context.Context, id string, status SessionStatus) error
    UpdateHeartbeat(ctx context.Context, id string, t time.Time) error
    Get(ctx context.Context, id string) (*SessionMeta, error)
    List(ctx context.Context, filter SessionFilter) ([]*SessionMeta, error)
}

type MessageStore interface {
    Append(ctx context.Context, msg *MessageRecord) error
    LoadBySession(ctx context.Context, sessionID string, afterSeq int) ([]*MessageRecord, error)
}

type SessionManagerInterface interface {
    NewSession(ctx context.Context, params NewSessionParams) (*RuntimeSession, string, error)
    GetCached(id string) (*RuntimeSession, bool)
    Resume(ctx context.Context, id string, mcpServers []MCPConfig) (*RuntimeSession, error)
    Close(ctx context.Context, id string) error
    MarkIdle(ctx context.Context, id string) error
    List(ctx context.Context, filter SessionFilter) ([]*SessionMeta, error)
    StartIdleScanner(ctx context.Context, interval, timeout time.Duration)
}

// ACP Handler 接口（由 Agent 实现）
type ACPAgent interface {
    Initialize(ctx context.Context, req *acp.InitializeRequest) (*acp.InitializeResponse, error)
    Authenticate(ctx context.Context, req *acp.AuthenticateRequest) (*acp.AuthenticateResponse, error)
    NewSession(ctx context.Context, req *acp.NewSessionRequest) (*acp.NewSessionResponse, error)
    Prompt(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error)
    ResumeSession(ctx context.Context, req *acp.ResumeSessionRequest) (*acp.ResumeSessionResponse, error)
    CloseSession(ctx context.Context, req *acp.CloseSessionRequest) (*acp.CloseSessionResponse, error)
    ListSessions(ctx context.Context, req *acp.ListSessionsRequest) (*acp.ListSessionsResponse, error)
    Cancel(ctx context.Context, req *acp.CancelRequest) (*acp.CancelResponse, error)
    HandleHeartbeat(ctx context.Context, req *acp.HeartbeatRequest) (*acp.HeartbeatResponse, error)
    HandleRelease(ctx context.Context, req *acp.ReleaseRequest) (*acp.ReleaseResponse, error)
}
```

---

## 6. 数据库设计

### 6.1 建表语句（参考，由 Ent 自动生成迁移）

```sql
CREATE TABLE sessions (
    id            TEXT PRIMARY KEY,
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'idle', 'closed')),
    user_id       INTEGER DEFAULT 0,
    username      TEXT DEFAULT '',
    business_id   TEXT DEFAULT '',
    business_type TEXT DEFAULT '',
    business_meta JSON DEFAULT '{}',
    mode               TEXT NOT NULL DEFAULT 'agent' CHECK (mode IN ('agent', 'plan')),
    heartbeat_interval INTEGER NOT NULL DEFAULT 10,
    summary            TEXT DEFAULT '',
    create_time   DATETIME NOT NULL,
    update_time   DATETIME NOT NULL
);

CREATE INDEX idx_sessions_status ON sessions(status);
CREATE INDEX idx_sessions_user_id ON sessions(user_id);
CREATE INDEX idx_sessions_business ON sessions(business_id, business_type);

CREATE TABLE session_messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT NOT NULL,
    seq          INTEGER NOT NULL,
    role         TEXT NOT NULL CHECK (role IN ('system', 'user', 'assistant', 'tool')),
    content      TEXT DEFAULT '',
    tool_calls   JSON DEFAULT '[]',
    tool_call_id TEXT DEFAULT '',
    create_time  DATETIME NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
    UNIQUE (session_id, seq)
);

CREATE INDEX idx_messages_session ON session_messages(session_id);
```

---

## 7. 关键流程

### 7.1 NewSession + Prompt 完整时序

```
Client                    Agent Server                   DB              MCP Server
  │                           │                           │                   │
  │──session/new─────────────►│                           │                   │
  │                           │──INSERT session──────────►│                   │
  │                           │◄──────────────────────────│                   │
  │                           │──Connect────────────────────────────────────►│
  │                           │◄───tools/list─────────────────────────────────│
  │                           │──Build ChatModelAgent─── │                   │
  │                           │──Cache in SessionManager │                   │
  │◄───{status:"active"}─────│                           │                   │
  │                           │                           │                   │
  │──session/prompt──────────►│                           │                   │
  │                           │──validate active────────  │                   │
  │                           │──INSERT messages─────────►│                   │
  │                           │──Run ReAct loop─────────  │                   │
  │                           │──LLM Call────────────────  │                   │
  │◄───SessionUpdate(文本块)──│                           │                   │
  │◄───SessionUpdate(文本块)──│                           │                   │
  │◄───RequestPermission──────│                           │                   │
  │───permission response────►│                           │                   │
  │                           │──CallTool───────────────────────────────────►│
  │                           │◄───tool result──────────────────────────────│
  │◄───SessionUpdate(结果)────│                           │                   │
  │                           │──...继续 ReAct...        │                   │
  │◄───{status:"completed"}───│                           │                   │
  │                           │──INSERT messages─────────►│                   │
```

### 7.2 Heartbeat 超时自动 Idle

示例：heartbeat_interval = 3s，超时 = 3 × 3 = 9s。

```
时间轴 (秒)：
  0s ─── Client 发送 _heartbeat ─── lastHeartbeat = now
  3s ─── Client 发送 _heartbeat ─── lastHeartbeat = now
  6s ─── Client 发送 _heartbeat ─── lastHeartbeat = now
  9s ─── Client 断开连接（崩溃/网络故障）
 10s ─── IdleScanner 扫描 ───────── lastHeartbeat = 6s，距 now 仅 4s，未超 9s，不处理
 13s ─── IdleScanner 扫描 ───────── lastHeartbeat = 6s，now = 13s，超过 9s
                                     → 标记为 idle，断开 MCP，更新 DB
 16s ─── Client 重连，session/resume → 重新连接 MCP，状态恢复为 active
```

### 7.3 优雅释放与恢复

```
客户端 A                           Agent Server                      客户端 B
  │                                   │                                  │
  │──prompt──► (推理进行中...)         │                                  │
  │                                   │                                  │
  │──_release──────────────────────► │                                  │
  │◄───────{ok}──────────────────────│                                  │
  │  (客户端 A 断开连接)               │  (Session 标记为 idle)            │
  │                                   │                                  │
  │                                   │  ◄────session/list───────────────│
  │                                   │  ────[{session_id, ...}]────────►│
  │                                   │                                  │
  │                                   │  ◄────session/resume─────────────│
  │                                   │  (从 DB 加载 + 重连 MCP)           │
  │                                   │  (状态: idle → active)            │
  │                                   │  ────SessionUpdate(流式)─────────►│
```

---

## 8. 配置设计

```yaml
# configs/config.yaml
server:
  name: agent-server
  version: "1.0.0"

transport:
  type: tcp          # stdio | tcp | unix
  tcp:
    listen: ":9090"
  unix:
    socket: "/var/run/acp.sock"

data:
  database:
    driver: sqlite3           # sqlite3 | postgres
    dsn: file:./data/acp.db?cache=shared&_journal_mode=WAL

llm:
  provider: openai
  api_key: ${LLM_API_KEY}     # 环境变量
  base_url: https://api.openai.com/v1
  model: gpt-4o
  context_window: 128000
  max_iterations: 50           # 服务端硬上限，session/new 传入的值不可超过此值

agent:
  default_max_iterations: 20   # 常量 DefaultMaxIterations，session/new 不传时使用此值

summarization:
  enabled: true
  trigger_ratio: 0.8           # 达到 context_window 的 80% 时触发摘要

idle_scanner:
  scan_interval: 10s

log:
  level: info
```

---

## 9. 迁移计划

从当前单文件 `package main` 架构迁移到上述 Kratos + Ent 架构，分阶段进行：

### Phase 1：基础设施搭建
- 初始化 Kratos 项目骨架
- 定义 Ent Schema（Session + SessionMessage）
- 生成 Ent Client 代码
- 编写 Kratos 配置文件

### Phase 2：Session 层重构
- 实现 SessionManager（基于 Ent Client）
- 实现 RuntimeSession
- 实现 IdleScanner
- 将现有 SQL 操作替换为 Ent 调用

### Phase 3：Agent 层重构
- 提取 Agent 结构体到 internal/agent
- 实现 ACPHandler（JSON-RPC 路由）
- 连接 Kratos Transport 层

### Phase 4：MCP/LLM 解耦
- 提取 MCPManager 到 internal/mcp
- 提取 LLM 工厂到 internal/llm
- 提取中间件到 internal/middleware

### Phase 5：Wire 依赖注入
- 编写 Wire 配置文件
- 编译时生成 DI 代码
- 端到端测试
