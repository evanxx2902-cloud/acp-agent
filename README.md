# ACP Agent Server

基于 [ACP (Agent Client Protocol)](https://agentclientprotocol.com) 的通用 AI Agent 服务器，使用 Go 语言编写。

- **AI 引擎**：[CloudWeGo eino](https://github.com/cloudwego/eino) — ChatModelAgent 做 ReAct 推理循环
- **通信协议**：[acp-go-sdk](https://github.com/coder/acp-go-sdk) — JSON-RPC 2.0 over stdio/TCP/Unix
- **工具系统**：[MCP (Model Context Protocol)](https://modelcontextprotocol.io) — 工具全部由客户端通过 MCP server 提供
- **中间件**：summarization（上下文压缩）+ plantask（任务管理）+ callbacks（可观测性）

## 架构

```
┌──────────────┐   JSON-RPC 2.0     ┌─────────────────────┐              ┌──────────────────┐
│  ACP 客户端   │ ◄── stdio/tcp ──► │ AgentSideConnection │              │   ChatModelAgent │
│  (IDE/CLI)   │                    │   (acp-go-sdk)      │              │   (eino adk)     │
└──────┬───────┘                    └──────────┬──────────┘              └────────┬─────────┘
       │                                      │                                  │
       │ NewSession.McpServers                │ buildSessionAgent()              │
       └──────────────────────────────────────┴──────────────────────────────────┘
                                    │
                    ┌───────────────┼───────────────┐
                    ▼               ▼               ▼
              summarization    plantask         callbacks
              (32K压缩)      (TaskCreate...)   (OnStart/End/Error)
```

工具调用链路：

```
LLM 输出 tool_call → MCPToolAdapter.InvokableRun()
  → SessionUpdate(StartToolCall)    ← ACP 通知客户端
  → RequestPermission               ← ACP 询问用户
  → mcpClient.CallTool(name, args)  ← MCP 协议执行
  → SessionUpdate(UpdateToolCall)   ← ACP 通知结果
  → 结果喂回 LLM
```

## 核心依赖

| 依赖 | 用途 |
|------|------|
| [acp-go-sdk](https://github.com/coder/acp-go-sdk) | ACP 协议：JSON-RPC 传输、会话管理、流式更新、权限控制 |
| [eino](https://github.com/cloudwego/eino) + [eino-ext](https://github.com/cloudwego/eino-ext) | AI 引擎：ChatModelAgent (ReAct)、流式输出、中间件体系 |
| [mcp-go](https://github.com/mark3labs/mcp-go) | MCP 客户端：连接 MCP server、发现工具、调用工具 |
| [lib/pq](https://github.com/lib/pq) | PostgreSQL 驱动（demo 中的数据库工具） |
| [clickhouse-go](https://github.com/ClickHouse/clickhouse-go) | ClickHouse 驱动（demo 中的数据库工具） |
| [modernc.org/sqlite](https://modernc.org/sqlite) | SQLite（session 持久化，纯 Go 无 CGO） |

## 快速开始

### 编译

```bash
go build -o agent-server .
```

### 运行

```bash
# 环境变量
export ACP_LLM_API_KEY="sk-..."
export ACP_LLM_MODEL="deepseek-chat"
export ACP_LLM_BASE_URL="https://api.deepseek.com/v1"
./agent-server

# 配置文件
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
| `ACP_LOG_LEVEL` | `log_level` | 日志级别 | `info` |

`listen` 支持三种格式：

| 值 | 说明 | 示例 |
|----|------|------|
| `stdio` | 标准输入输出（默认） | `./agent-server` |
| `tcp://host:port` | TCP 监听，多客户端并发 | `tcp://0.0.0.0:8080` |
| `unix:///path` | Unix domain socket | `unix:///tmp/acp.sock` |

## ACP 协议支持

### Agent 方法

| 方法 | 状态 | 说明 |
|------|------|------|
| `initialize` | ✅ | 协议版本协商，能力声明（LoadSession、MCP HTTP/SSE、Session Close/List/Resume） |
| `authenticate` | ✅ | 无认证，直接返回成功 |
| `session/new` | ✅ | 创建 session，连接 MCP server 发现工具 |
| | | `_meta.system_prompt` — 动态注入系统提示词 |
| | | `_meta.max_iterations` — 覆盖 ReAct 最大迭代次数 |
| `session/load` | ✅ | 从 SQLite 恢复 session，`_meta.mcpServers` 重连 MCP |
| `session/prompt` | ✅ | 流式输出 + 思考内容 + 工具调用 + 权限询问 |
| `session/cancel` | ✅ | 取消当前正在执行的 turn |
| `session/close` | ✅ | 关闭 session，释放 MCP 连接 |
| `session/list` | ✅ | 从 SQLite 查询所有 session |
| `session/resume` | ✅ | 从 DB 恢复并继续对话 |
| `session/set_mode` | ✅ | `agent`（默认）/ `plan`（注入规划指令到 system prompt） |
| `session/set_config_option` | ⚠️ | 接受但不生效，预留扩展点 |
| `logout` | ✅ | 无认证，直接返回成功 |

### Agent → Client 通知

| 通知 | 说明 |
|------|------|
| `AgentMessageChunk` | LLM 流式文本，逐 token 推送 |
| `AgentThoughtChunk` | LLM 推理内容（extended thinking） |
| `StartToolCall` | 工具调用开始（含参数） |
| `UpdateToolCall` | 工具状态变更（pending → in_progress → completed/failed，含结果预览） |
| `RequestPermission` | 工具执行前请求用户授权 |

## 功能特性

| 功能 | 说明 |
|------|------|
| 流式输出 | LLM 生成内容以 chunk 实时推送 |
| 思考内容 | `reasoning_content` 映射为 ACP `AgentThoughtChunk` |
| 多轮对话 | 会话历史写穿 SQLite，重启不丢失 |
| 多会话并发 | 每个 session 独立 MCP 连接 + ChatModelAgent |
| Per-session 工具 | 不同 session 可连接不同 MCP server，工具集隔离 |
| 上下文压缩 | summarization 中间件：超过 32K tokens 自动压缩历史 |
| 任务管理 | plantask 中间件：TaskCreate/Get/Update/List 工具 |
| 可观测性 | callbacks：OnStart/OnEnd/OnError 全局钩子 |
| 取消操作 | context 取消传播到 eino runner |
| 权限控制 | 每个工具调用前 ACP 协议询问客户端 |
| 多传输 | stdio / TCP / Unix socket |

### Session 模式

通过 `session/set_mode` 切换。

**Agent 模式（默认）**：

```
用户输入 → ReAct 推理循环（思考 + 工具调用 + 再思考）→ 返回结果
```

**Plan 模式**：

`set_mode("plan")` 后，system prompt 自动注入规划指令。LLM 收到任务时先调用 `TaskCreate` 拆解步骤，展示计划，等用户确认后再执行。无需额外代码分支——模式切换只是 prompt 变化。

```bash
./demo -y --mode plan
> 帮我重构项目错误处理
# LLM 先 TaskCreate → 展示计划 → 等待确认
> 开始执行
# LLM 逐步执行，实时更新 Task 状态
```

## 项目结构

```
acp/
├── agent.go             # 入口 + EinoAgent (ACP 接口) + 消息转换 + 流式 + callbacks
├── mcp.go               # MCP 客户端管理器 + ToolAdapter (MCP→eino，含 ACP 通知/权限)
├── llm.go               # 配置加载 + LLM ChatModel 工厂
├── session.go           # Session + SessionManager + SQLite Store
├── examples/
│   └── demo/
│       ├── main.go            # Demo 入口 + ACP 客户端（交互式）
│       ├── server.go          # MCP server 编排
│       ├── tools_fs.go        # 8 个 filesystem 工具
│       ├── tools_terminal.go  # run_command
│       ├── tools_pg.go        # PostgreSQL 查询 + 表列表
│       ├── tools_ch.go        # ClickHouse 查询 + 表列表
│       └── helpers.go         # 公共工具函数
├── go.mod / go.sum
└── README.md
```

## Agent 启动到响应完整流程

```
1. ACP 客户端连接 → JSON-RPC over stdio/tcp/unix
2. initialize → 协议协商，agent 声明能力
3. session/new → 连接 MCP server，发现工具，构建 per-session ChatModelAgent
     ├─ summarization 中间件 — 上下文超 32K/100条自动压缩
     ├─ plantask 中间件 — 注入 TaskCreate/Get/Update/List 工具
     └─ callbacks — OnStart/OnEnd/OnError 全局可观测
4. session/prompt → ContentBlock 转 eino Message → ReAct 循环
     ├─ LLM 输出文本 → AgentMessageChunk 流式推送
     ├─ LLM 输出 reasoning → AgentThoughtChunk 推送
     └─ LLM 调用工具 → StartToolCall → RequestPermission → CallTool(MCP) → UpdateToolCall
5. 返回 PromptResponse(stopReason=end_turn)，新消息写穿 SQLite
```

### MCP 传输方式

客户端在 `session/new` 时通过 `McpServers` 指定 MCP server 连接方式。

| 传输 | 配置 |
|------|------|
| Stdio | `{"type":"stdio","command":"./mcp-server","args":[]}` |
| HTTP | `{"type":"http","url":"https://mcp.example.com","headers":[]}` |
| SSE | `{"type":"sse","url":"https://mcp.example.com/sse","headers":[]}` |

## 测试

### demo 客户端

demo 是自包含的测试工具，同时充当 MCP server 和 ACP client。

**准备工作**：

```bash
cd /home/evan/acp
go build -o agent-server .
cat > config.json << 'EOF'
{
  "llm_api_key": "sk-你的key",
  "llm_base_url": "https://api.deepseek.com/v1",
  "llm_model": "deepseek-chat"
}
EOF
```

**Agent 模式**：

```bash
go run ./examples/demo/ -y
> 列出当前目录的文件
```

**Plan 模式**：

```bash
go run ./examples/demo/ -y --mode plan
> 帮我重构项目
# LLM 先输出计划，用户说"开始"后执行
```

**自定义参数**：

```bash
go run ./examples/demo/ -y \
  --system-prompt "你是 Go 专家" \
  --max-iter 10 \
  --mode plan
```

**交互式权限**（不用 `-y`）：

```bash
go run ./examples/demo/
> 写文件到 /tmp/test.txt
# 弹出权限确认框，输入 y/n
```

**客户端命令**：

| 命令 | 说明 |
|------|------|
| `/help` | 显示帮助 |
| `/resume` | 从 DB 恢复中断的会话继续执行 |
| `quit` | 退出 |

**TCP / Unix Socket**：

```bash
# TCP
./agent-server -config config.json -listen tcp://0.0.0.0:8080
go run ./examples/demo/ -y --connect tcp://localhost:8080

# Unix socket
./agent-server -config config.json -listen unix:///tmp/acp.sock
go run ./examples/demo/ -y --connect unix:///tmp/acp.sock
```

## 扩展指南

### 添加新工具类别（demo 侧）

新建 `examples/demo/tools_xxx.go`：

```go
func registerXxxTools(s *server.MCPServer) {
    s.AddTool(mcp.NewTool("tool_name",
        mcp.WithDescription("..."),
        mcp.WithString("arg1", mcp.Required()),
    ), handleXxx)
}

func handleXxx(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
    return mcp.NewToolResultText("result"), nil
}
```

在 `server.go` 注册：

```go
func registerAllTools(s *server.MCPServer) {
    registerFSTools(s)
    registerTerminalTools(s)
    registerPGTools(s)
    registerCHTools(s)
    registerXxxTools(s)  // ← 新增
}
```

agent 端不需要任何改动——MCP 协议自动完成工具发现和调用。

## 许可

Apache 2.0
