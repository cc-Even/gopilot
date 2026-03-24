# 🚀 Gopilot

> A terminal-first coding copilot built in Go, featuring task management, AI teammates, skills, and git worktrees.

Gopilot 是一个专为真实开发工作流打造的终端 AI 助手。它不仅仅是一个简单的问答工具，而是能够真正帮你**拆分任务、执行代码、跟踪进度并完成收尾**，完美契合开发者的日常工作节奏。

与传统的单轮对话 CLI 不同，Gopilot 将**任务看板、多 Agent 协作、技能扩展、后台命令执行以及 Git Worktree 管理**无缝整合到了同一个终端界面中。我们的目标是：让代码任务从“描述需求”到“组织执行”，都能在一个地方高效完成。

## ✨ Why Gopilot?

- **💻 Terminal-first**: 原生运行在终端中，完美融入你的本地仓库和日常编码心流。
- **🔄 Multi-step execution**: 采用 Planner / Executor 双阶段执行模式，告别一次性回答，真正解决复杂问题。
- **🛠 Built for coding workflows**: 内置任务看板、Teammate 消息机制、后台任务管理和 Worktree 隔离。
- **🧩 Extensible by skills**: 通过 `skills/` 目录轻松扩展特定场景的 AI 能力。
- **🧠 Configurable subagents**: 通过 `subagents/` 目录在启动时自动注入可路由的子代理。
- **⚡ Implemented in Go**: 编译为单一二进制文件，部署极简，非常适合作为本地工具使用。

## 🌟 Features

- 🤖 **交互式编码代理**: 基于 OpenAI Chat Completions 打造。
- 🖥️ **现代化 TUI 界面**: 清晰的输出区、日志区和命令输入区。
- 📋 **强大的任务管理**: 支持任务的创建、查询、依赖关系处理及状态流转。
- 🤝 **Teammate 协作**: 任务派发、状态等待、消息回传与自动收尾。
- 🌳 **Git Worktree 管理**: 为每个任务分配隔离的工作目录，互不干扰。
- ⚙️ **后台命令执行**: 支持后台运行命令并轮询执行结果。
- 🎯 **Skills 扩展机制**: 支持按主题加载自定义扩展能力。
- 🧠 **SubAgent 注入机制**: 支持通过配置文件在启动时注册可委派的子代理。
- 🧪 **高可靠性**: 核心行为均有 Go 单元测试覆盖。

## 🚀 Quick Start

### 1. Requirements

- Go `1.25` 或更高版本
- Git
- 一个可用的 兼容 OpenAI API 的 API Key 和接口地址
- 操作系统：Linux / macOS / Windows

### 2. Configuration

复制环境变量模板并填入你的配置：

```bash
cp .env.example setting.env
```

编辑 `setting.env`：

```dotenv
OPENAI_API_KEY=your_api_key_here
MODEL=gpt-4o-mini
# 可选：自定义 OpenAI 兼容接口地址
OPENAI_BASE_URL=https://api.openai.com/v1
```

### 3. Build & Run

编译并运行：

```bash
make build
./gopilot
```

或者直接使用快捷命令：

```bash
make run
```

## ⌨️ Command Reference

启动 Gopilot 后，你可以直接输入自然语言描述你的任务。此外，还支持以下内置命令：

- `/model <name>`：切换当前使用的模型，并自动保存到环境文件中。
- `/tasks`：查看当前的任务看板。
- `/team`：查看 Teammate 的工作状态。
- `/stop`：中断当前主 Agent 的任务。
- `/clear`：清空界面并重置当前会话。
- `Ctrl+C`：退出程序。

## ⚙️ Configuration Details

程序启动时，会在可执行文件所在目录按以下顺序查找并加载环境文件：

1. `.env`
2. `setting.env`

**核心配置项：**
- `OPENAI_API_KEY`：你的 OpenAI API 密钥。
- `MODEL`：默认使用的模型名称（未设置时默认为 `gpt-4o-mini`）。
- `OPENAI_BASE_URL`：（可选）自定义的 OpenAI 兼容接口地址。

> **💡 提示**：当前的技能目录和环境文件都是基于“可执行文件所在目录”进行解析的。因此，相比于直接使用 `go run .`，我们更推荐先 `make build` 编译后再运行二进制文件。

## 🧩 Skills 与 SubAgents

程序启动时，会从可执行文件所在目录自动扫描以下扩展目录：

- `skills/**/SKILL.md`
- `subagents/**/SUBAGENT.md`

其中 `skills` 用于按需加载知识/流程片段，`subagents` 用于注册可被 `route_to_subagent` 工具调用的子代理。

### SubAgent 文件格式

每个 SubAgent 使用一个独立目录，文件名固定为 `SUBAGENT.md`。支持的 frontmatter 字段如下：

- `name`：子代理名称；未填写时默认使用目录名。
- `description`：子代理用途说明；会暴露给主 agent 作为路由提示。
- `model`：可选；指定该子代理自己的模型。未填写时动态继承调用它的父 Agent 当前模型。

正文内容会作为该子代理的 `system prompt`。

示例：

```md
---
name: code-reviewer
description: Focus on regression and test gaps
model: gpt-4.1-mini
---
You are a code review specialist.
Prioritize bug risk, behavioral regressions, and missing tests.
Return concise findings with file references when possible.
```

推荐目录结构：

```text
subagents/
└── code-reviewer/
    └── SUBAGENT.md
```

完成配置后，重启程序即可自动注入该 SubAgent，主 agent 会在可用工具中看到它，并可通过 `route_to_subagent` 将子任务委派给它。

## 📁 Project Layout

```text
.
├── main.go                 # TUI 入口文件
├── pkg/agents/             # Agent、任务、Team、Worktree 及 Skills 的核心实现
├── pkg/version/            # 版本信息注入
├── skills/                 # 内置技能目录
├── subagents/              # 启动时自动加载的 SubAgent 配置目录
├── Makefile                # 构建与测试脚本
├── .env.example            # 环境变量配置模板
└── .github/workflows/      # CI/CD 工作流配置
```
## 📝 License

This project is licensed under the MIT License. See the `LICENSE` file for details.
