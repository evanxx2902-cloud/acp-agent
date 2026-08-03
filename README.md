# Agent Server 技术方案文档

## 1. 概述

Agent Server 是一个基于 ACP（Agent-Client Protocol）协议的 AI Agent 服务端。它通过 Unix Socket 接收客户端连接，每个连接对应一个会话，管理会话生命周期，编排 LLM + MCP 工具的 ReAct 推理循环，并将结果流式返回给客户端。

**核心设计：一个连接 = 一个会话。** 连接 open 即 session active，连接 close 即 session idle。无需心跳或显式释放。

### 1.1 技术栈

| 组件 | 选型 | 说明 |
|------|------|------|
| ORM | [Ent](https://entgo.io/) | 类型安全的 Go ORM，自动生成数据模型代码 |
| 存储 | SQLite（开发）/ PostgreSQL（生产） | 通过 Ent 的数据库驱动适配 |
| AI 框架 | [Eino](https://github.com/cloudwego/eino) | 字节跳动开源的 AI Agent 框架，提供 ChatModelAgent、ReAct、Runner |
| LLM | OpenAI 兼容 API | 通过 eino-ext 的 OpenAI adapter |
| MCP | [mcp-go](https://github.com/mark3labs/mcp-go) | Model Context Protocol 客户端库 |
| 协议 | ACP (JSON-RPC 2.0) | 通过 acp-go-sdk |
| 传输 | Unix Socket | 本节点内通信，连接断开即 session 释放 |

### 1.2 工程结构

```
acp/
├── main.go              # 启动入口 + Kratos 初始化 + Wire 注入
├── agent.go             # Agent 核心 + ACP 协议 Handler（实现 acp.Agent 接口）
├── runner.go            # ReAct 推理循环
├── session.go           # Session 实体 + RuntimeSession + SessionManager
├── mcp.go               # MCPManager + ToolAdapter + MCPConfig
├── llm.go               # LLMConfigProvider + ModelInfoProvider + ChatModel 工厂
├── middleware.go         # 对话摘要 + Plan 模式中间件
├── callback.go          # Eino 回调（日志、telemetry）
├── config.go            # Config 结构体 + 加载
├── ent/
│   ├── schema/
│   │   ├── session.go           # Session 实体定义
│   │   └── session_message.go   # SessionMessage 实体定义
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
package schema

import (
    "time"
    "entgo.io/ent"
    "entgo.io/ent/schema/field"
    "entgo.io/ent/schema/index"
)

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
package schema

import (
    "time"
    "entgo.io/ent"
    "entgo.io/ent/schema/field"
    "entgo.io/ent/schema/edge"
    "entgo.io/ent/schema/index"
)

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
```

### 2.3 ER 关系图

```
┌──────────────────────────────────────────┐
│                 Session                  │
├──────────────────────────────────────────┤
│  id                   TEXT PK (UUID v4)  │
│  status               TEXT               │
│  user_id              INTEGER            │
│  username             TEXT               │
│  business_id          TEXT               │
│  business_type        TEXT               │
│  business_meta        JSON               │
│  mode                 TEXT               │
│  summary              TEXT               │
│  create_time          TIMESTAMP          │
│  update_time          TIMESTAMP          │
│                                          │
│  CHECK: status IN ('active','idle','closed')  │
│  CHECK: mode IN ('agent','plan')         │
└────────────────┬─────────────────────────┘
                 │ 1
                 │
                 │ *
┌────────────────▼─────────────────────────┐
│              SessionMessage              │
├──────────────────────────────────────────┤
│  id               INTEGER PK (auto-incr)   │
│  session_id       TEXT FK → Session      │
│  seq              INTEGER                │
│  role             TEXT                   │
│  content          TEXT                   │
│  tool_calls       JSON                   │
│  tool_call_id     TEXT                   │
│  create_time      TIMESTAMP              │
│                                          │
│  UNIQUE: (session_id, seq)               │
│  CHECK: role IN ('system','user','assistant','tool') │
└──────────────────────────────────────────┘
```

### 2.4 数据归属

**Session 表持久化的字段：**

| 字段 | 说明 |
|------|------|
| `id`, `status`, `mode` | 会话核心标识和状态 |
| `user_id`, `username` | 会话归属 |
| `business_id`, `business_type`, `business_meta` | 业务上下文 |
| `summary` | 对话摘要 |

**每次请求由客户端携带的数据：**

| 数据 | 携带接口 |
|------|----------|
| `mcp_servers` | session/new, session/resume |

> `system_prompt` 不存 session 表——`session/new` 时作为 `session_messages` 第一条记录（seq=0, role=system）持久化。

---

## 3. ACP 协议接口设计

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
| `capabilities` | object | 服务端支持的能力集（streaming, mcp, plan） |

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
| `summarization_trigger_ratio` | float64 | 否 | 摘要触发比例，默认 `0.8`。当前 token 数超过 `context_window × ratio` 时触发 |
| `business_id` | string | 否 | 业务上下文标识 |
| `business_type` | string | 否 | 业务上下文类型 |
| `business_meta` | object | 否 | 扩展业务元数据 |

**响应字段：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `session_id` | string | 服务端生成的 UUID v4，后续所有操作的主键 |
| `status` | string | `"active"` |

**职责：**
- 调用 `LLMConfigProvider.GetConfig()` 获取 LLM 配置
- 调用 `ModelInfoProvider.GetContextWindow()` 获取上下文长度
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
- 验证当前连接持有该 Session（`active` 且属于本连接）
- 验证 Session 没有正在执行的 prompt（同一 session 同时只能有一个 prompt）
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
>
> 如果当前连接已持有一个 active session，调用 `session/resume` 会自动释放前一个 session（标记为 idle），再恢复目标 session。

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

## 4. Session 状态机

### 4.1 设计约束

**一个连接一个会话。** Agent 同时只持有一个 session。连接 open 即 session active，连接 close 即 session idle（自动触发，无需客户端显式操作）。

### 4.2 状态定义

| 状态 | 含义 | 允许的操作 |
|------|------|------------|
| `active` | 被某个连接独占，禁止同一 session 并发 prompt | prompt, cancel, close |
| `idle` | 无连接持有，MCP 已释放，消息和 summary 保留在内存 | resume, close |
| `closed` | 不可逆终态 | 无 |

### 4.3 状态流转图

```
                       session/new
                          │
                          ▼
                    ┌──────────┐       session/close
                    │          │──────────────────────┐
        ┌──────────►│  ACTIVE  │                      │
        │           │          │                      │
        │           └────┬─────┘                      │
        │                │                            │
        │                │ 连接断开（自动）              │
        │                │ (断开 MCP，标记 idle)         │
        │                │                            │
        │                ▼                            ▼
        │           ┌──────────┐               ┌──────────┐
        │           │          │ session/close │          │
        └───────────│   IDLE   │──────────────►│  CLOSED  │
    session/resume  │          │               │          │
                    └──────────┘               └──────────┘
```

### 4.4 状态转换表

| 当前状态 | 触发事件 | 目标状态 | 说明 |
|----------|----------|----------|------|
| - | `session/new` | `active` | 新建会话，绑定到当前连接 |
| `active` | `session/prompt` | `active` | 保持活跃，执行推理 |
| `active` | `session/cancel` | `active` | 取消推理，仍可继续对话 |
| `active` | 连接断开 | `idle` | 内核通知断开，自动断开 MCP，标记 idle |
| `active` | `session/close` | `closed` | 断开 MCP，清理内存 |
| `idle` | `session/resume` | `active` | 从 DB 加载，重连 MCP |
| `idle` | `session/close` | `closed` | 清理内存 |
| `closed` | 任何操作 | `closed` | 终态，拒绝所有操作 |

---

## 5. 核心结构体定义与关系

### 5.1 结构体关系全景图

```
App
├── EntClient
├── ACPServer
│   └── Agent
└── SessionManager
    └── RuntimeSession
        ├── ChatModel
        ├── MCPManager
        │   └── ToolAdapter
        └── Messages
```

- 每接受一个连接创建一个 `Agent`，`Agent` 实现 `acp.Agent` 接口，同时只持有一个 session
- `SessionManager` 是全局的，管理所有 session 的缓存和持久化
- 每个 `RuntimeSession` 对应一个会话，持有独立的 LLM 客户端、MCP 连接、消息历史

### 5.2 各结构体职责总结

| 结构体 | 职责 |
|--------|------|
| `App` | 启动入口，组装依赖 |
| `ACPServer` | 管理 ACP transport，接收客户端连接，创建 Agent |
| `Agent` | 处理单个 ACP 连接的协议方法 |
| `SessionManager` | 全局 Session 内存缓存、持久化、列表查询 |
| `RuntimeSession` | 单会话运行时：消息历史、MCP 连接、LLM 客户端 |
| `MCPManager` | MCP 客户端生命周期、工具发现 |
| `ToolAdapter` | MCP Tool → Eino InvokableTool |
| `EntClient` | DB Client |

---

## 6. 数据库设计

### 6.1 建表语句

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

### 7.2 断开自动 Idle + 恢复

```
客户端 A                           Agent Server                      客户端 B
  │                                   │                                  │
  │──session/new──► (active)          │                                  │
  │──prompt──► (推理进行中...)         │                                  │
  │                                   │                                  │
  │  (客户端 A 断开连接)               │                                  │
  │                                   │──conn.Done() → MarkIdle         │
  │                                   │  (断开 MCP，标记 idle)            │
  │                                   │                                  │
  │                                   │  ◄────session/list───────────────│
  │                                   │  ────[{session_id, ...}]────────►│
  │                                   │                                  │
  │                                   │  ◄────session/resume─────────────│
  │                                   │  (从 DB 加载 + 重连 MCP)           │
  │                                   │  (状态: idle → active)            │
  │                                   │  ────SessionUpdate(流式)─────────►│
```

### 7.3 并发冲突

**跨连接冲突：** A 持有 active session，B 尝试操作。

```
客户端 A                              Agent Server                       客户端 B
  │                                      │                                   │
  │──session/resume──► (X: idle→active)   │                                   │
  │──prompt──► (推理中...)                │                                   │
  │                                      │    ◄────session/resume────────────│
  │                                      │    ──{error: active in another}──►│
  │                                      │    ◄────session/prompt────────────│
  │                                      │    ──{error: does not belong}────►│
  │  (客户端 A 断开)                      │                                   │
  │                                      │    ◄────session/resume────────────│
  │                                      │    ──{ok}────────────────────────►│
```

**同连接并发 prompt：** A 对同一 session 发送两个 prompt。

```
客户端 A                              Agent Server
  │                                      │
  │──prompt(session X)──► (推理中...)     │
  │──prompt(session X)──►                │
  │◄──{error: session busy}──────────────│
  │◄──{completed}──────── (第一个完成)    │
  │──prompt(session X)──► (可以了)        │
```

- 跨连接：active 状态的 session 拒绝其他连接的所有操作
- 同连接并发：session 级别的 prompt 互斥锁，同一时刻只允许一个 prompt 执行

---

## 8. 配置设计

```yaml
server:
  name: agent-server
  version: "1.0.0"

transport:
  type: tcp
  tcp:
    listen: ":9090"
  unix:
    socket: "/var/run/acp.sock"

data:
  database:
    driver: sqlite3
    dsn: file:./data/acp.db?cache=shared&_journal_mode=WAL

log:
  level: info
```

### 8.1 LLMConfigProvider 接口

```go
type LLMConfigProvider interface {
    GetConfig(ctx context.Context) (*LLMConfig, error)
}

type LLMConfig struct {
    Provider string
    APIKey   string
    BaseURL  string
    Model    string
}

type ModelInfoProvider interface {
    GetContextWindow(ctx context.Context, model string) (int, error)
}
```

### 8.2 摘要触发

摘要始终开启。触发条件：`当前 token 数 > context_window × summarization_trigger_ratio`。

- `context_window`：由 `ModelInfoProvider.GetContextWindow()` 获取
- `summarization_trigger_ratio`：由客户端在 `session/new` 时传入，默认 `0.8`
