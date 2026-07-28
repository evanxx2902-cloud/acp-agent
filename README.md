# ACP Agent Server

基于 [ACP (Agent Client Protocol)](https://agentclientprotocol.com) 的通用 AI Agent 服务器，使用 Go 语言编写。将 [CloudWeGo eino](https://github.com/cloudwego/eino) 作为 AI 推理引擎，通过 [acp-go-sdk](https://github.com/coder/acp-go-sdk) 提供标准化的 JSON-RPC 2.0 通信协议。

## 架构

```
┌──────────────┐    stdio/NDJSON     ┌─────────────────────┐     Go API     ┌──────────────────┐
│  ACP 客户端   │ ◄────────────────► │ AgentSideConnection │ ◄────────────► │   EinoAgent      │
│  (IDE/CLI)   │   JSON-RPC 2.0     │   (acp-go-sdk)      │               │   (我们的实现)    │
└──────────────┘                     └─────────────────────┘               └────────┬─────────┘
                                                                                    │
                                                                                    ▼
                                                                           ┌──────────────────┐
                                                                           │  ChatModelAgent  │
                                                                           │  (eino adk)      │
                                                                           │  ReAct 推理循环   │
                                                                           └────────┬─────────┘
                                                                                    │
                                                                                    ▼
                                                                           ┌──────────────────┐
                                                                           │   LLM 后端       │
                                                                           │  OpenAI 兼容 API │
                                                                           └──────────────────┘
```

### 核心依赖

| 依赖 | 版本 | 用途 |
|------|------|------|
| [acp-go-sdk](https://github.com/coder/acp-go-sdk) | v0.13.5 | ACP 协议实现：JSON-RPC 传输、会话管理、流式更新 |
| [eino](https://github.com/cloudwego/eino) | v0.9.13 | AI Agent 框架：ChatModelAgent、工具调用、流式输出 |
| [eino-ext/openai](https://github.com/cloudwego/eino-ext) | v0.1.13 | OpenAI 兼容的 ChatModel 实现 |

## 快速开始

### 编译

```bash
go build -o agent-server .
```

### 运行

```bash
# 使用 OpenAI
export ACP_LLM_API_KEY="sk-..."
export ACP_LLM_MODEL="gpt-4o"
./agent-server

# 使用 DeepSeek
export ACP_LLM_API_KEY="sk-..."
export ACP_LLM_BASE_URL="https://api.deepseek.com/v1"
export ACP_LLM_MODEL="deepseek-chat"
./agent-server

# 使用 Ollama (本地模型)
export ACP_LLM_BASE_URL="http://localhost:11434/v1"
export ACP_LLM_MODEL="qwen2.5"
export ACP_LLM_API_KEY="ollama"  # Ollama 不需要真实的 API Key
./agent-server
```

### JSON 配置文件 (可选)

```bash
./agent-server -config config.json
```

`config.json` 示例：

```json
{
  "llm_provider": "openai-compatible",
  "llm_api_key": "sk-...",
  "llm_base_url": "https://api.deepseek.com/v1",
  "llm_model": "deepseek-chat",
  "system_prompt": "你是一个专业的编程助手，擅长 Go 语言开发。",
  "max_iterations": 20
}
```

## 配置说明

| 环境变量 | JSON 字段 | 说明 | 默认值 |
|----------|-----------|------|--------|
| `ACP_LLM_API_KEY` | `llm_api_key` | LLM API 密钥 | - |
| `ACP_LLM_MODEL` | `llm_model` | 模型名称 | `gpt-4o` |
| `ACP_LLM_BASE_URL` | `llm_base_url` | API 地址 | `https://api.openai.com/v1` |
| `ACP_LLM_PROVIDER` | `llm_provider` | 提供商类型 | `openai-compatible` |
| `ACP_SYSTEM_PROMPT` | `system_prompt` | 系统提示词 | 通用助手提示词 |

- 环境变量优先级高于 JSON 配置文件
- 支持任何兼容 OpenAI API 的 LLM 服务（OpenAI、DeepSeek、Groq、Ollama、vLLM 等）

## 功能

### 已实现

- **多轮对话**：自动维护会话历史，上下文在同一个 session 内持续累积
- **流式输出**：实时将 LLM 生成结果以 chunk 形式推送给客户端
- **推理内容**：支持 `reasoning_content`（Claude extended thinking 等），映射为 ACP 的 `AgentThoughtChunk`
- **文件读写**：Agent 可以调用 `acp_read_file` / `acp_write_file` 工具操作客户端文件系统
- **取消操作**：客户端可随时取消正在执行的任务
- **多会话并发**：支持同时维护多个独立的 session
- **ACL 权限**：写文件操作会触发客户端的权限确认流程

### ACP 协议支持

| 方法 | 状态 |
|------|------|
| `initialize` | 完整支持 |
| `authenticate` | 通过（无认证） |
| `session/new` | 完整支持 |
| `session/prompt` | 完整支持（流式 + 工具调用） |
| `session/cancel` | 完整支持 |
| `session/close` | 完整支持 |
| `session/list` | 未实现 |
| `session/load` | 未实现 |
| `session/resume` | 未实现 |
| `session/set_mode` | 接受但不生效 |
| `session/set_config_option` | 接受但不生效 |
| `logout` | 未实现 |

## 项目结构

```
acp/
├── main.go              # 入口：配置加载、依赖注入、启动服务
├── agent.go             # EinoAgent：实现 acp.Agent 接口
├── session.go           # Session + SessionManager（SQLite 持久化）
├── store.go             # SQLite 持久化层
├── config.go            # 配置结构体及加载逻辑（环境变量 + JSON 文件）
├── llm.go               # LLM ChatModel 工厂
├── messages.go          # ACP ContentBlock ↔ eino Message 转换
├── streaming.go         # eino AgentEvent → ACP SessionUpdate 流式桥接
├── tools.go             # baseTools：read_file / write_file / run_command
├── mcp_adapter.go       # MCP 工具 → eino BaseTool 适配器
├── mcp_manager.go       # MCP 客户端生命周期管理
├── go.mod
└── go.sum
```

## 工作流程

1. **客户端连接**：通过 stdio 建立 JSON-RPC 2.0 连接
2. **初始化**：客户端发送 `initialize`，服务器返回协议版本和能力声明
3. **创建会话**：客户端调用 `session/new`，服务器创建 session 并注入系统提示词
4. **发送提示**：客户端调用 `session/prompt` 附带 `ContentBlock` 列表
5. **消息转换**：服务器将 ACP 的 `ContentBlock` 转换为 eino 的 `Message` 格式
6. **Agent 执行**：使用 eino 的 `ChatModelAgent` 进行 ReAct 推理循环
7. **流式输出**：将 LLM 的流式生成结果实时转换为 ACP `SessionUpdate` 通知
8. **工具调用**：当 LLM 决定调用工具时，通过 ACP 协议与客户端交互执行
9. **返回结果**：推理完成后返回 `PromptResponse`，结束本轮对话

## 测试

acp-go-sdk 自带一个 ACP 客户端示例，可以直接用来验证 agent 是否正常工作。

### 1. 准备工作

```bash
# 编译 agent
cd /home/evan/acp
go build -o agent-server .

# 创建配置文件（包含 API Key，不要提交到 git）
cat > config.json << 'EOF'
{
  "llm_api_key": "sk-你的key",
  "llm_base_url": "https://api.deepseek.com/v1",
  "llm_model": "deepseek-chat"
}
EOF
```

### 2. 运行测试

```bash
cd /home/evan/go/pkg/mod/github.com/coder/acp-go-sdk@v0.13.5/example/client/
go run main.go /home/evan/acp/agent-server -config /home/evan/acp/config.json
```

客户端会自动启动 agent 作为子进程，通过 stdio 进行完整的 ACP 交互：

1. `initialize` — 握手，协商协议版本
2. `session/new` — 创建会话
3. `session/prompt` — 发送 "Hello, agent!" 并接收流式回复
4. 过程中如有工具调用、权限请求，客户端会打印并交互式处理

### 3. 预期输出

```
✅ Connected to agent (protocol v1)
📝 Created session: sess_xxxxxxxx
💬 User: Hello, agent!

 Hello! I'm an AI assistant with access to your filesystem...

✅ Agent completed
```

- 看到 `✅ Connected` 说明 ACP 握手成功
- 看到 LLM 的回复内容说明 eino 推理链路正常
- 看到 `✅ Agent completed` 说明整个 turn 正常结束

### 4. 测试工具调用（可选）

把客户端代码里的 prompt 改成需要读文件的内容来验证工具调用。编辑 client 的 `main.go`：

```go
// 将
Prompt: []acp.ContentBlock{acp.TextBlock("Hello, agent!")},

// 改为
Prompt: []acp.ContentBlock{acp.TextBlock("帮我读取 /home/evan/acp/README.md 的内容")},
```

重新运行测试，应该能看到 agent 调用 `acp_read_file` 工具读取文件内容。

## 许可

Apache 2.0
