# ACP Agent Server

通用的 AI Agent 服务器，支持 [ACP (Agent Client Protocol)](https://agentclientprotocol.com)。

**引擎** [CloudWeGo eino](https://github.com/cloudwego/eino) — ChatModelAgent 做 ReAct 循环。
**工具** [MCP (Model Context Protocol)](https://modelcontextprotocol.io) — 全部由客户端提供，agent 不内置任何工具。

## 架构

```
┌─────────────┐  JSON-RPC 2.0    ┌─────────────────┐               ┌──────────────────┐
│  ACP Client │ ◄── stdio/tcp ─► │  EinoAgent      │               │  ChatModelAgent  │
│  (IDE/CLI)  │    unix socket   │  (agent.go)     │──── ReAct ──►│  (eino adk)      │
└──────┬──────┘                  └────────┬────────┘               └────────┬─────────┘
       │                                 │                                  │
       │ NewSession._meta                │ middleware:                      │
       │   system_prompt                 │   summarization (压缩)           │
       │   max_iterations                │   plantask (任务管理)            │
       │   business fields               │   callbacks (可观测)             │
       │                                 │                                  │
       │ NewSession.McpServers           │ buildSessionAgent()              │
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
go build -o agent-server .
export ACP_LLM_API_KEY="sk-..." ACP_LLM_MODEL="deepseek-chat" ACP_LLM_BASE_URL="https://api.deepseek.com/v1"
./agent-server
```

## 配置

`config.json`（只存 LLM 配置）：

```json
{
  "llm_api_key": "sk-...",
  "llm_base_url": "https://api.deepseek.com/v1",
  "llm_model": "deepseek-chat",
  "context_window": 131072
}
```

其余通过代码默认值 + 环境变量覆盖：

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `ACP_LLM_API_KEY` | - | LLM API 密钥 |
| `ACP_LLM_MODEL` | `gpt-4o` | 模型名称 |
| `ACP_LLM_BASE_URL` | `https://api.openai.com/v1` | API 地址 |
| `ACP_CONTEXT_WINDOW` | `131072` | 模型上下文窗口 |
| `ACP_SYSTEM_PROMPT` | `You are a helpful AI assistant.` | 默认系统提示词 |
| `ACP_SUMMARIZATION_TRIGGER` | `0.5` | 压缩触发比例 (50%) |
| `ACP_LISTEN` | `stdio` | 监听方式 |
| `ACP_LOG_LEVEL` | `info` | 日志级别 |
| `ACP_DATA_DIR` | `~/.acp-agent` | 数据目录 |
| `ACP_DB_PATH` | `$DATA_DIR/sessions.db` | SQLite 路径 |

`listen` 格式：`stdio` | `tcp://0.0.0.0:8080` | `unix:///tmp/acp.sock`

## ACP 接口支持

| 方法 | 说明 |
|------|------|
| `initialize` | 能力声明：LoadSession、Session Close/List/Resume、MCP HTTP/SSE |
| `authenticate` | 无认证，直接返回成功 |
| `session/new` | 创建 session，连接 MCP server 发现工具 |
| `session/load` | 从 SQLite 恢复 session |
| `session/prompt` | 流式输出 + 工具调用 + 权限询问 |
| `session/cancel` | 取消当前 turn |
| `session/close` | 关闭 session（status → closed） |
| `session/list` | 查询所有 session |
| `session/resume` | 恢复中断的会话（idle → active） |
| `session/set_mode` | 切换 `agent` / `plan` 模式 |
| `session/set_config_option` | 接受但不生效 |
| `logout` | 无认证，直接返回成功 |

### session/new._meta 字段

| 字段 | 类型 | 说明 |
|------|------|------|
| `system_prompt` | string | 覆盖默认系统提示词 |
| `max_iterations` | number | 覆盖最大迭代次数 |
| `user_id` | string | 用户 ID |
| `username` | string | 用户名 |
| `business_type` | string | 业务类型 |
| `business_id` | string | 关联业务 ID |
| `custom_*` | any | `metadata` JSON 列透传 |

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

## 存储

### sessions 表

| 列 | 类型 | 说明 |
|----|------|------|
| `id` | TEXT PK | session ID |
| `status` | TEXT | `active` → `idle` → `closed` |
| `mode` | TEXT | `agent` / `plan` |
| `user_id` | TEXT | 用户 ID |
| `username` | TEXT | 用户名 |
| `business_type` | TEXT | 业务类型 |
| `business_id` | TEXT | 关联业务 ID |
| `metadata` | TEXT | JSON，扩展字段透传 |
| `created_at` | INTEGER | 创建时间 |
| `updated_at` | INTEGER | 更新时间 |

### session_messages 表

| 列 | 类型 | 说明 |
|----|------|------|
| `id` | INTEGER PK | 自增 |
| `session_id` | TEXT FK | 关联 session |
| `seq` | INTEGER | 消息序号 |
| `role` | TEXT | `system` / `user` / `assistant` / `tool` |
| `content` | TEXT | 文本内容，tool 消息存 `(completed)` |
| `tool_calls` | TEXT | JSON，assistant 调用工具时填充 |
| `tool_call_id` | TEXT | tool 消息的结果 ID |
| `tool_name` | TEXT | tool 消息的工具名 |
| `created_at` | INTEGER | 创建时间 |

**消息存储策略**：thinking 和工具结果全文不持久化（只流式推送给客户端），仅存轻量状态。

## Session 生命周期

```
session/new → active ──(agent关闭)──→ idle ──(resume)──→ active
                     │                                    │
                     └──(session/close)──→ closed ←────────┘
```

| 状态 | 能做什么 |
|------|---------|
| `active` | prompt / cancel / set_mode / close |
| `idle` | load / resume / close |
| `closed` | 终态，所有操作拒绝 |

## 项目结构

```
acp/
├── agent.go    # 入口 + EinoAgent + 消息转换 + 流式 + callbacks
├── mcp.go      # MCP 客户端 + ToolAdapter (权限/通知)
├── llm.go      # 配置 + LLM 工厂
├── session.go  # Session + SessionManager + SQLite Store
└── examples/demo/
    ├── main.go            # ACP 客户端 + 交互式终端
    ├── server.go          # MCP server 编排
    ├── tools_fs.go        # 8 个 filesystem 工具
    ├── tools_terminal.go  # run_command
    ├── tools_pg.go        # PostgreSQL
    ├── tools_ch.go        # ClickHouse
    └── helpers.go
```

## 测试

```bash
go run ./examples/demo/ -y
> 列出项目文件

go run ./examples/demo/ -y --mode plan
> 帮我重构 agent.go

go run ./examples/demo/ -y --connect tcp://localhost:8080

go run ./examples/demo/ -y --max-iter 10
```

## 许可

Apache 2.0
