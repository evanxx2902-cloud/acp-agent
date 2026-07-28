# ACP Agent Server

基于 [ACP (Agent Client Protocol)](https://agentclientprotocol.com) 的通用 AI Agent 服务器，使用 Go 语言编写。

- **AI 引擎**：[CloudWeGo eino](https://github.com/cloudwego/eino) — ChatModelAgent 做 ReAct 推理循环
- **通信协议**：[acp-go-sdk](https://github.com/coder/acp-go-sdk) — JSON-RPC 2.0 over stdio
- **工具系统**：[MCP (Model Context Protocol)](https://modelcontextprotocol.io) — 工具全部由客户端通过 MCP server 提供，agent 自身不包含任何硬编码工具

## 架构

```
┌──────────────┐    stdio/NDJSON     ┌─────────────────────┐     Go API     ┌──────────────────┐
│  ACP 客户端   │ ◄────────────────► │ AgentSideConnection │ ◄────────────► │   EinoAgent      │
│  (IDE/CLI)   │   JSON-RPC 2.0     │   (acp-go-sdk)      │               │   (agent.go)     │
└──────┬───────┘                     └─────────────────────┘               └────────┬─────────┘
       │                                                                           │
       │  McpServers                                                               ▼
       └───────────────────────────────────────────────────────────────────  ChatModelAgent
                                                                              (eino adk)
                                                                              ReAct 推理循环
                                                                                    │
                                                                                    ▼
                                                                            LLM 后端
                                                                            OpenAI 兼容 API
```

工具调用链路：

```
LLM 输出 tool_call → eino 路由到 MCPToolAdapter
  → ACP 通知客户端 (StartToolCall)
  → ACP 询问权限 (RequestPermission)
  → MCP 协议执行 (CallTool)
  → ACP 通知结果 (UpdateToolCall)
  → 结果喂回 LLM
```

## 核心依赖

| 依赖 | 用途 |
|------|------|
| [acp-go-sdk](https://github.com/coder/acp-go-sdk) | ACP 协议：JSON-RPC 传输、会话管理、流式更新、权限控制 |
| [eino](https://github.com/cloudwego/eino) + [eino-ext](https://github.com/cloudwego/eino-ext) | AI 引擎：ChatModelAgent (ReAct)、流式输出、工具调用 |
| [mcp-go](https://github.com/mark3labs/mcp-go) | MCP 客户端：连接 MCP server、发现工具、调用工具 |
| [lib/pq](https://github.com/lib/pq) | PostgreSQL 驱动（demo 中的数据库工具使用） |
| [modernc.org/sqlite](https://modernc.org/sqlite) | SQLite（session 持久化，纯 Go 无 CGO） |

## 快速开始

### 编译

```bash
go build -o agent-server .
```

### 配置

```bash
# 方式一：环境变量
export ACP_LLM_API_KEY="sk-..."
export ACP_LLM_MODEL="deepseek-chat"
export ACP_LLM_BASE_URL="https://api.deepseek.com/v1"
./agent-server

# 方式二：配置文件
cat > config.json << 'EOF'
{
  "llm_api_key": "sk-...",
  "llm_base_url": "https://api.deepseek.com/v1",
  "llm_model": "deepseek-chat"
}
EOF
./agent-server -config config.json
```

### 配置说明

| 环境变量 | JSON 字段 | 说明 | 默认值 |
|----------|-----------|------|--------|
| `ACP_LLM_API_KEY` | `llm_api_key` | LLM API 密钥 | - |
| `ACP_LLM_MODEL` | `llm_model` | 模型名称 | `gpt-4o` |
| `ACP_LLM_BASE_URL` | `llm_base_url` | API 地址 | `https://api.openai.com/v1` |
| `ACP_LLM_PROVIDER` | `llm_provider` | 提供商 | `openai-compatible` |
| `ACP_SYSTEM_PROMPT` | `system_prompt` | 默认系统提示词（可被 session 覆盖） | 通用助手 |
| `ACP_DATA_DIR` | `data_dir` | 数据目录 | `~/.acp-agent` |
| `ACP_DB_PATH` | `db_path` | SQLite 路径 | `$DATA_DIR/sessions.db` |
| `ACP_LISTEN` | `listen` | 传输模式 | `stdio` |

支持任何兼容 OpenAI API 的 LLM 服务（OpenAI、DeepSeek、Groq、Ollama、vLLM 等）。

### 传输模式

`ACP_LISTEN` 环境变量或 `listen` JSON 字段控制 agent 接受客户端连接的方式。

| 值 | 说明 | 示例 |
|----|------|------|
| `stdio` | 标准输入输出（默认） | `./agent-server` |
| `tcp://host:port` | TCP 监听，支持多客户端并发 | `./agent-server -listen tcp://0.0.0.0:8080` |
| `unix:///path` | Unix domain socket | `./agent-server -listen unix:///tmp/acp.sock` |

## ACP 协议支持

### 方法

| 方法 | 状态 | 说明 |
|------|------|------|
| `initialize` | ✅ 完整 | 协议版本协商、能力声明（图片输入、session 加载、MCP over HTTP/SSE） |
| `authenticate` | ✅ 通过 | 无认证，直接返回成功 |
| `session/new` | ✅ 完整 | 创建 session，自动连接 MCP server 发现工具，支持 `_meta.system_prompt` 动态注入提示词 |
| `session/load` | ✅ 完整 | 从 SQLite 恢复 session |
| `session/prompt` | ✅ 完整 | 流式输出（AgentMessageChunk）+ 思考内容（AgentThoughtChunk）+ 工具调用 + 权限控制 |
| `session/cancel` | ✅ 完整 | 取消当前正在执行的 turn |
| `session/close` | ✅ 完整 | 关闭 session，释放 MCP 连接，删除持久化数据 |
| `session/list` | ✅ 完整 | 从 SQLite 查询所有 session |
| `session/resume` | ✅ 完整 | plan 模式下，确认计划并恢复执行 |
| `session/set_mode` | ✅ 完整 | 支持 `agent` 模式（默认）和 `plan` 模式 |
| `session/set_config_option` | ⚠️ 接受 | 接受但不生效，预留扩展点 |
| `logout` | ❌ 未实现 | 无认证机制，不需要登出 |

### 通知（Agent → Client）

| 通知 | 状态 | 说明 |
|------|------|------|
| `AgentMessageChunk` | ✅ | LLM 流式文本输出 |
| `AgentThoughtChunk` | ✅ | LLM 推理内容（如 extended thinking） |
| `StartToolCall` | ✅ | 工具调用开始通知 |
| `UpdateToolCall` | ✅ | 工具调用状态更新（pending → in_progress → completed/failed） |
| `UpdatePlan` | ✅ | plan 模式下的计划步骤通知 |
| `RequestPermission` | ✅ | 工具执行前的权限确认 |

### 功能

| 功能 | 说明 |
|------|------|
| 流式输出 | LLM 生成内容以 chunk 实时推送给客户端 |
| 思考内容 | 支持 `reasoning_content`，映射为 ACP `AgentThoughtChunk` |
| 多轮对话 | 自动维护会话历史，写穿 SQLite 持久化 |
| 多会话并发 | 每个 session 独立的 MCP 连接 + ChatModelAgent |
| Per-session 工具 | 不同 session 可连接不同 MCP server，工具集相互隔离 |
| 取消操作 | context 取消传播到 eino runner |
| 权限控制 | 每个工具调用前通过 ACP 协议询问客户端 |

### Session 模式

通过 `session/set_mode` 切换，modeId 为 `agent` 或 `plan`。

**Agent 模式（默认）**：

```
用户输入 → ReAct 推理循环（思考+工具调用+再思考）→ 返回结果
```

**Plan 模式**：

```
用户输入 → LLM 创建执行计划（无工具）→ UpdatePlan 通知客户端
                                        → 等用户确认
用户确认 (g) → ResumeSession → 按计划逐步执行 ReAct（有工具）
```

## 项目结构

```
acp/
├── agent.go             # 入口 + EinoAgent (ACP 接口实现) + 消息转换 + 流式输出
├── mcp.go               # MCP 客户端管理器 + ToolAdapter (MCP→eino 工具适配)
├── llm.go               # 配置加载 + LLM ChatModel 工厂
├── session.go           # Session + SessionManager + SQLite Store
├── examples/
│   └── demo/
│       ├── main.go      # Demo 入口 + ACP 客户端 + 交互式权限
│       ├── server.go    # MCP server 编排
│       ├── tools_fs.go       # 8 个 filesystem 工具
│       ├── tools_terminal.go # run_command
│       ├── tools_pg.go       # PostgreSQL 查询 + 表列表
│       └── helpers.go        # 公共工具函数
├── go.mod / go.sum
└── README.md
```

## 工作流程

```
1. ACP 客户端连接 → JSON-RPC over stdio
2. initialize → 协议版本协商
3. session/new → agent 连接 MCP server，发现工具，构建 per-session ChatModelAgent
4. session/prompt → 用户消息 → ContentBlock 转 eino Message → ReAct 循环
   ├─ LLM 输出文本 → AgentMessageChunk 流式推送
   ├─ LLM 输出 reasoning → AgentThoughtChunk 推送
   └─ LLM 调用工具 → StartToolCall → RequestPermission → CallTool(MCP) → UpdateToolCall
5. 返回 PromptResponse(stopReason=end_turn)
```

### MCP 传输方式

Agent 支持连接 4 种类型的 MCP server，客户端在 `session/new` 时通过 `McpServers` 字段指定。

| 传输类型 | 状态 | 配置字段 | 说明 |
|---------|------|---------|------|
| Stdio | ✅ | `McpServer.Stdio{Command, Args}` | 启动子进程，通过 stdin/stdout 通信 |
| HTTP | ✅ | `McpServer.Http{Url, Headers}` | Streamable HTTP 协议 |
| SSE | ✅ | `McpServer.Sse{Url, Headers}` | Server-Sent Events 协议 |
| ACP | ⚠️ | `McpServer.Acp{Id}` | 通过 ACP 组件传输（协议尚不稳定） |

## 测试

### 使用 demo 客户端（推荐）

demo 是一个独立的可执行程序，同时充当 MCP server 和 ACP client，无需额外配置。

**准备工作**：

```bash
cd /home/evan/acp

# 编译 agent
go build -o agent-server .

# 创建配置文件
cat > config.json << 'EOF'
{
  "llm_api_key": "sk-你的key",
  "llm_base_url": "https://api.deepseek.com/v1",
  "llm_model": "deepseek-chat"
}
EOF
```

**Agent 模式测试**：

```bash
go run ./examples/demo/ -y

> 列出当前目录的文件                              # agent 模式下直接执行，LLM 自主调用工具
```

**注入自定义系统提示词**：

```bash
go run ./examples/demo/ -y --system-prompt "你是一个专业的 Go 代码审查员，只回答 Go 相关的问题。"
```

**Plan 模式测试**：

```bash
go run ./examples/demo/ -y -mode plan

[plan] > 帮我重构整个项目的错误处理               # LLM 先输出执行计划
📋 Plan:
  ○ 分析当前的错误处理方式
  ○ 定义统一的错误类型
  ○ 逐文件替换错误处理
  ...

[plan] > g                                        # 输入 g 确认执行，触发 ResumeSession
```

**交互式权限测试（不用 -y）**：

```bash
go run ./examples/demo/

> 写一个文件到 /tmp/test.txt

┌─ Tool Permission ───────────────────┐
│ write_file(path=/tmp/test.txt, content=hello)
│ args: {"content":"hello","path":"/tmp/test.txt"}
└──────────────────────────────────────┘
Allow? [y/N]                                       # 输入 y 放行，n 拒绝
```

### 使用 acp-go-sdk 自带客户端

```bash
cd /home/evan/go/pkg/mod/github.com/coder/acp-go-sdk@v0.13.5/example/client/
go run main.go /home/evan/acp/agent-server -config /home/evan/acp/config.json
```

> **注意**：acp-go-sdk 自带客户端不在 `NewSession` 中配置 `McpServers`，因此 agent 会以零工具的状态运行。如需测试工具调用，推荐使用 demo 客户端。

### TCP / Unix Socket 测试

**TCP 模式**：

```bash
# 终端 1：启动 agent，监听 TCP 端口
./agent-server -config config.json -listen tcp://0.0.0.0:8080

# 终端 2：demo 客户端通过 TCP 连接
go run ./examples/demo/ -y --connect tcp://localhost:8080
```

**Unix Socket 模式**：

```bash
# 终端 1：启动 agent，监听 Unix socket
./agent-server -config config.json -listen unix:/tmp/acp.sock

# 终端 2：demo 客户端通过 socket 连接
go run ./examples/demo/ -y --connect unix:///tmp/acp.sock
```

## 扩展指南

### 添加新的工具类别（demo 侧）

新增 `examples/demo/tools_xxx.go`：

```go
func registerXxxTools(s *server.MCPServer) {
    s.AddTool(mcp.NewTool("tool_name",
        mcp.WithDescription("..."),
        mcp.WithString("arg1", mcp.Required()),
    ), handleXxx)
}

func handleXxx(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
    // 实现逻辑
    return mcp.NewToolResultText("result"), nil
}
```

然后在 `server.go` 的 `registerAllTools` 中加一行：

```go
func registerAllTools(s *server.MCPServer) {
    registerFSTools(s)
    registerTerminalTools(s)
    registerDBTools(s)
    registerXxxTools(s)  // ← 新增这一行
}
```

agent 端不需要任何改动——工具发现和调用全部通过 MCP 协议自动完成。

## 许可

Apache 2.0
