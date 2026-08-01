# ACP Agent Server

通用的 AI Agent 服务器，支持 [ACP (Agent Client Protocol)](https://agentclientprotocol.com)。

**引擎** [CloudWeGo eino](https://github.com/cloudwego/eino) — ChatModelAgent 做 ReAct 循环。
**工具** [MCP (Model Context Protocol)](https://modelcontextprotocol.io) — 全部由客户端提供，agent 不内置任何工具。
**存储** [Ent ORM](https://entgo.io) — 类型安全的数据库访问层。

## 架构

```
┌─────────────┐  JSON-RPC 2.0    ┌─────────────────┐               ┌──────────────────┐
│  ACP Client │ ◄── stdio/tcp ─► │  Agent           │               │  ChatModelAgent  │
│  (IDE/CLI)  │    unix socket   │  (agent.go)      │──── ReAct ──►│  (eino adk)      │
└──────┬──────┘                  └────────┬────────┘               └────────┬─────────┘
       │                                 │                                  │
       │ session/new._meta               │ middleware:                      │
       │   system_prompt                 │   summarization (压缩)           │
       │   mode                          │   plantask (任务管理)            │
       │   max_iterations                │   callbacks (可观测)             │
       │   heartbeat_interval            │                                  │
       │   business_*                    │ buildSessionAgent()              │
       │                                 │                                  │
       │ session/new.McpServers          │ LLMConfigProvider (per-session)  │
       └─────────────────────────────────┴──────────────────────────────────┘
                                                  │
                                          MCP (stdio/http/sse)
                                                  │
                                          ┌───────┴───────┐
                                          │  MCP Server    │
                                          │  (客户端提供)   │
                                          └───────────────┘
```

## 快速开始

```bash
# 构建
go build -o agent-server .

# 通过环境变量配置 LLM
export ACP_LLM_API_KEY="sk-..." ACP_LLM_MODEL="deepseek-chat" ACP_LLM_BASE_URL="https://api.deepseek.com/v1"

# stdio 模式
./agent-server

# 或指定 YAML 配置 + LLM JSON 配置
./agent-server -config configs/config.yaml -llm-config config.json
```

## 配置

### 服务端配置 (`configs/config.yaml`)

```yaml
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
    driver: sqlite3
    dsn: file:./data/acp.db?cache=shared&_journal_mode=WAL&_fk=1

log:
  level: info       # debug | info | warn | error
```

### LLM 配置

通过 `LLMConfigProvider` 接口获取，默认实现优先读取 JSON 文件，再读取环境变量：

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `ACP_LLM_PROVIDER` | `openai-compatible` | LLM 提供商 |
| `ACP_LLM_API_KEY` | - | LLM API 密钥 |
| `ACP_LLM_MODEL` | `gpt-4o` | 模型名称 |
| `ACP_LLM_BASE_URL` | `https://api.openai.com/v1` | API 地址 |
| `ACP_CONTEXT_WINDOW` | 根据模型自动选择 | 上下文窗口大小 |

`listen` 格式：`stdio` | `tcp://0.0.0.0:8080` | `unix:///tmp/acp.sock`

## ACP 接口支持

| 方法 | 说明 |
|------|------|
| `initialize` | 能力声明：Session Close/List/Resume、MCP HTTP/SSE |
| `authenticate` | 无认证，直接返回成功 |
| `session/new` | **服务端生成 UUID v4 session_id**，连接 MCP server 发现工具，system_prompt 持久化为 seq=0 消息，mode 创建后不可变 |
| `session/prompt` | 流式输出 + 工具调用 + 权限询问，同 session 互斥（拒绝并发 prompt） |
| `session/cancel` | 取消当前 turn |
| `session/close` | 关闭 session（→ closed），断开 MCP，停止心跳定时器 |
| `session/list` | 查询所有 session，支持分页和多条件过滤 |
| `session/resume` | 从 DB 恢复 idle session，重连 MCP |
| `session/set_mode` | **已废弃** — mode 在 session/new 时指定，创建后不可变 |
| `session/set_config_option` | 接受但不生效 |
| `logout` | 无认证，直接返回成功 |
| `_heartbeat` | 扩展方法：重置心跳定时器（超时时间 = 3 × heartbeat_interval） |
| `_release` | 扩展方法：主动标记 session 为 idle，断开 MCP |

### session/new._meta 字段

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `system_prompt` | string | - | 系统提示词，持久化为 seq=0 消息，不可变 |
| `mode` | string | `agent` | `agent` 或 `plan`，创建后不可变 |
| `max_iterations` | number | 20 | 单次推理最大轮次 |
| `heartbeat_interval` | number | 10 | 心跳间隔（秒），超时 = 3x |
| `summarization_trigger_ratio` | number | 0.8 | 摘要触发比例 |
| `business_id` | string | - | 业务标识 |
| `business_type` | string | - | 业务类型 |
| `business_meta` | object | - | 扩展业务元数据 |

### Agent → Client 通知

| 通知 | 说明 |
|------|------|
| `AgentMessageChunk` | LLM 流式文本 |
| `AgentThoughtChunk` | LLM 推理内容（不持久化） |
| `StartToolCall` | 工具调用开始 |
| `UpdateToolCall` | 工具状态更新（含结果摘要） |
| `RequestPermission` | 工具执行前权限确认 |

## Session 模式

| 模式 | 行为 |
|------|------|
| `agent`（默认） | 收到任务直接执行 |
| `plan` | system prompt 注入规划指令，LLM 先用 TaskCreate 拆步骤，确认后执行 |

## Session 生命周期

```
session/new → active ──(heartbeat超时/_release)──→ idle ──(resume)──→ active
                    │                                               │
                    └────(session/close)──→ closed ←────────────────┘
```

| 状态 | 能做什么 |
|------|---------|
| `active` | prompt / cancel / close / heartbeat |
| `idle` | resume / close |
| `closed` | 终态，所有操作拒绝 |

**心跳机制**：每个 active session 持有独立的 `time.Timer`，超时 = `3 × heartbeat_interval`。
- `_heartbeat` → `timer.Reset()`
- `_release` / 超时 → 断开 MCP、停定时器、标记 idle

**并发控制**：
- 跨连接：`lockedBy` 归属校验 — 其他连接无法 prompt 不属于自己的 active session
- 同连接：`promptMu` 互斥锁 — 同一 session 同时只能有一个 prompt

## 存储 (Ent ORM)

### sessions 表

| 列 | 类型 | 说明 |
|----|------|------|
| `id` | TEXT PK | session ID（UUID v4，服务端生成） |
| `status` | TEXT | `active` → `idle` → `closed` |
| `mode` | TEXT | `agent` / `plan`，创建后不可变 |
| `user_id` | INTEGER | 用户 ID |
| `username` | TEXT | 用户名 |
| `business_id` | TEXT | 业务标识 |
| `business_type` | TEXT | 业务类型 |
| `business_meta` | JSON | 扩展业务元数据 |
| `heartbeat_interval` | INTEGER | 心跳间隔（秒） |
| `summary` | TEXT | 对话摘要 |
| `locked_by` | TEXT | 当前持有连接 ID |
| `locked_at` | DATETIME | 持有时间 |
| `create_time` | DATETIME | 创建时间 |
| `update_time` | DATETIME | 更新时间 |

### session_messages 表

| 列 | 类型 | 说明 |
|----|------|------|
| `id` | INTEGER PK | 自增 |
| `session_id` | TEXT FK | 关联 session |
| `seq` | INTEGER | 消息序号（seq=0 为 system_prompt）|
| `role` | TEXT | `system` / `user` / `assistant` / `tool` |
| `content` | TEXT | 文本内容，tool 消息存 `(completed)` |
| `tool_calls` | JSON | assistant 调用工具时填充 |
| `tool_call_id` | TEXT | tool 消息的结果 ID |
| `create_time` | DATETIME | 创建时间 |

**消息存储策略**：thinking 和工具结果全文不持久化，仅存轻量状态。

## 可扩展接口

项目提供以下接口，可通过依赖注入替换实现：

```go
type LLMConfigProvider interface {
    GetConfig(ctx context.Context) (*LLMConfig, error)
}

type ModelInfoProvider interface {
    GetContextWindow(ctx context.Context, model string) (int, error)
}
```

默认实现从 JSON 文件 + 环境变量读取，可替换为数据库、配置中心等方式。

## 项目结构

```
acp/
├── main.go              # 入口 + App 依赖组装
├── agent.go             # Agent 实现 acp.Agent 接口
├── session.go           # RuntimeSession + SessionManager (Ent + 定时器)
├── mcp.go               # MCP 客户端 + ToolAdapter
├── llm.go               # LLMConfigProvider 接口 + 默认实现
├── config.go            # 配置结构体 + YAML 加载
├── runner.go            # runReAct() + 流式事件处理
├── middleware.go         # summarization + plantask 中间件
├── callback.go          # eino 可观测回调
├── configs/
│   └── config.yaml      # 服务端配置文件
├── ent/
│   ├── schema/          # Ent schema 定义
│   └── *.go             # Ent 生成代码
└── examples/demo/
    ├── main.go          # ACP 客户端 + 交互式终端
    ├── server.go        # MCP server 编排
    ├── tools_fs.go      # 8 个 filesystem 工具
    ├── tools_terminal.go  # run_command
    ├── tools_pg.go      # PostgreSQL
    ├── tools_ch.go      # ClickHouse
    └── helpers.go
```

## 测试

```bash
go run ./examples/demo/ -y
> 列出项目文件

go run ./examples/demo/ -y --mode plan
> 帮我重构 agent.go

go run ./examples/demo/ -y --connect tcp://localhost:9090

go run ./examples/demo/ -y --max-iter 10
```

## 许可

Apache 2.0
