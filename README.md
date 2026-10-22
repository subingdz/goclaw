<p align="center">
  <img src="_statics/goclaw-logo.svg" alt="GoClaw" height="200" />
</p>

<p align="center"><strong>多租户 AI 智能体平台</strong></p>

<p align="center">
用 Go 构建的多智能体 AI 网关。20 多个大语言模型提供商。7 个消息渠道。多租户 PostgreSQL。<br/>
单一二进制文件。经过生产验证。智能体会主动为你做编排。
</p>

<p align="center">
  <a href="https://docs.goclaw.sh">文档</a> •
  <a href="https://docs.goclaw.sh/#quick-start">快速开始</a> •
  <a href="https://x.com/nlb_io">Twitter / X</a>
</p>

<p align="center">
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go_1.26-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go" /></a>
  <a href="https://www.postgresql.org/"><img src="https://img.shields.io/badge/PostgreSQL_18-316192?style=flat-square&logo=postgresql&logoColor=white" alt="PostgreSQL" /></a>
  <a href="https://www.docker.com/"><img src="https://img.shields.io/badge/Docker-2496ED?style=flat-square&logo=docker&logoColor=white" alt="Docker" /></a>
  <a href="https://developer.mozilla.org/en-US/docs/Web/API/WebSocket"><img src="https://img.shields.io/badge/WebSocket-010101?style=flat-square&logo=socket.io&logoColor=white" alt="WebSocket" /></a>
  <a href="https://opentelemetry.io/"><img src="https://img.shields.io/badge/OpenTelemetry-000000?style=flat-square&logo=opentelemetry&logoColor=white" alt="OpenTelemetry" /></a>
  <a href="https://www.anthropic.com/"><img src="https://img.shields.io/badge/Anthropic-191919?style=flat-square&logo=anthropic&logoColor=white" alt="Anthropic" /></a>
  <a href="https://openai.com/"><img src="https://img.shields.io/badge/OpenAI_Compatible-412991?style=flat-square&logo=openai&logoColor=white" alt="OpenAI" /></a>
  <img src="https://img.shields.io/badge/License-CC%20BY--NC%204.0-lightgrey?style=flat-square" alt="许可证：CC BY-NC 4.0" />
</p>

**GoClaw** 是一个多智能体 AI 网关，将大语言模型连接到你的工具、渠道和数据 —— 以单个 Go 二进制文件部署，零运行时依赖。它跨 20 多个大语言模型提供商编排智能体团队和跨智能体委托，并提供完整的多租户隔离。

这是 [OpenClaw](https://github.com/openclaw/openclaw) 的 Go 移植版本，具备增强的安全性、多租户 PostgreSQL 支持以及生产级可观测性。

🌐 **语言：**
[🇺🇸 English](_readmes/README.en.md) ·
[🇻🇳 Tiếng Việt](_readmes/README.vi.md) ·
[🇯🇵 日本語](_readmes/README.ja.md) ·
[🇰🇷 한국어](_readmes/README.ko.md) ·
[🇵🇭 Tagalog](_readmes/README.tl.md) ·
[🇪🇸 Español](_readmes/README.es.md) ·
[🇧🇷 Português](_readmes/README.pt.md) ·
[🇮🇹 Italiano](_readmes/README.it.md) ·
[🇩🇪 Deutsch](_readmes/README.de.md) ·
[🇫🇷 Français](_readmes/README.fr.md) ·
[🇸🇦 العربية](_readmes/README.ar.md) ·
[🇮🇳 हिन्दी](_readmes/README.hi.md) ·
[🇷🇺 Русский](_readmes/README.ru.md) ·
[🇧🇩 বাংলা](_readmes/README.bn.md) ·
[🇮🇱 עברית](_readmes/README.he.md) ·
[🇵🇱 Polski](_readmes/README.pl.md) ·
[🇨🇿 Čeština](_readmes/README.cs.md) ·
[🇳🇱 Nederlands](_readmes/README.nl.md) ·
[🇹🇷 Türkçe](_readmes/README.tr.md) ·
[🇺🇦 Українська](_readmes/README.uk.md) ·
[🇮🇩 Bahasa Indonesia](_readmes/README.id.md) ·
[🇹🇭 ไทย](_readmes/README.th.md) ·
[🇵🇰 اردو](_readmes/README.ur.md) ·
[🇷🇴 Română](_readmes/README.ro.md) ·
[🇸🇪 Svenska](_readmes/README.sv.md) ·
[🇬🇷 Ελληνικά](_readmes/README.el.md) ·
[🇭🇺 Magyar](_readmes/README.hu.md) ·
[🇫🇮 Suomi](_readmes/README.fi.md) ·
[🇩🇰 Dansk](_readmes/README.da.md) ·
[🇳🇴 Norsk](_readmes/README.nb.md)

## 关于本 fork

本仓库是 [GoClaw](https://github.com/nextlevelbuilder/goclaw) 的**国内生态改造版**:功能对齐上游 `dev`,并在此之上把国产模型、国产消息渠道与国内网关的实际行为做成一等公民,而不是靠外部脚本凑。下表随每个功能提交逐行增长。

| 能力 | 面向国内的点 | 配置入口 |
|---|---|---|
| 向量与检索 | embedding 维度**以数据库列为准**:启动即读取 pgvector 列宽,provider 声明的宽度与列不一致时直接拒绝启动并打印修复命令;`goclaw migrate embed-dims --to N` 可重设定(先 `--dry-run` 列出宽度 / 待清空行数 / 索引数)。留空表示"不发送截断参数",使 bge-m3 这类原生宽度已匹配的模型可用 | 系统设置 → Embedding;Providers → Embedding |
| 语音转写 | `audio.stt` 成为唯一真配置面(OpenAI 兼容端点，如 SiliconFlow SenseVoiceSmall)、`timeout_ms` 真正生效、QQ/微信语音的 **SILK** 由外部解码器处理(ffmpeg 解不了 SILK，启动日志会说明有没有找到)；密钥只走 `config_secrets` / `GOCLAW_AUDIO_STT_API_KEY`。两个面板的 stt 表单此前是"假配置面"，现在都写 `audio.stt` | 配置 → Integrations → Speech-to-Text；桌面端 内置工具 → stt |
| 技能生态 | ClawHub / GitHub 社区技能源（默认关闭）：搜索→人工检视（文件清单、可执行文件、声明依赖、缺失依赖、是否需要 shell、安全结论只给原因和行号）→安装；装完不授权给任何 agent、不自动 pip/npm 装依赖，内容风险硬拒；agent 侧 `skill_install` 是两步协议且拒绝 cron/心跳/子 agent 发起 | 技能 → 社区；`skills.registries.*` |

## 核心特性

- **8 阶段智能体流水线** — 上下文 → 历史 → 提示词 → 思考 → 行动 → 观察 → 记忆 → 摘要。阶段可插拔，执行路径始终启用
- **4 种提示词模式** — Full / Task / Minimal / None，支持分段门控、缓存边界优化和按会话解析模式
- **3 层记忆体系** — 工作记忆（当前对话）→ 情景记忆（会话摘要）→ 语义记忆（知识图谱）。L0/L1/L2 渐进式加载，L0 自动注入
- **知识仓库（Knowledge Vault）** — 带 `[[wikilinks]]` 的文档注册表、混合检索（FTS + pgvector）、文件系统同步
- **智能体团队与编排** — 共享任务板、跨智能体委托（同步/异步）、3 种编排模式（auto / explicit / manual）
- **自我演化** — 指标 → 建议 → 带护栏的自动适配。智能体会打磨自己的沟通风格
- **多租户 PostgreSQL** — 每用户工作空间、每用户上下文文件、API 密钥加密（AES-256-GCM）、RBAC、会话隔离
- **20 多个大语言模型提供商** — Anthropic（原生 HTTP+SSE，支持提示词缓存）、OpenAI、OpenRouter、Requesty、Groq、DeepSeek、Gemini、Mistral、xAI、MiniMax、Cohere、Perplexity、DashScope、百炼（Bailian）、Zai、Ollama、Ollama Cloud、Vertex AI、Claude CLI、Codex、ACP，以及任何 OpenAI 兼容端点
- **7 个消息渠道** — Telegram、Discord、Slack、Zalo OA、Zalo Personal、飞书/Lark、WhatsApp
- **Extended Thinking** — 按提供商启用思考模式（Anthropic 预算 token、OpenAI 推理力度、DashScope 思考预算），支持流式输出
- **Heartbeat** — 通过 HEARTBEAT.md 检查清单进行定期智能体签到，支持正常时静默、活跃时段、重试逻辑和渠道投递
- **调度与定时任务** — `at`、`every` 和 cron 表达式用于自动化智能体任务，支持基于渠道的并发
- **生产级安全** — 5 层权限体系（网关认证 → 全局工具策略 → 每智能体 → 每渠道 → 仅限所有者），外加速率限制、提示词注入检测、SSRF 防护、Shell 拒绝模式和 AES-256-GCM 加密
- **单一二进制文件** — 约 25 MB 静态 Go 二进制文件，无 Node.js 运行时，启动时间 <1 秒，可在 5 美元 VPS 上运行
- **可观测性** — 内置大语言模型调用追踪（含 span 和提示词缓存指标），可选 OpenTelemetry OTLP 导出

## Claw 生态系统

|                 | OpenClaw        | ZeroClaw | PicoClaw | **GoClaw**                              |
| --------------- | --------------- | -------- | -------- | --------------------------------------- |
| 语言            | TypeScript      | Rust     | Go       | **Go**                                  |
| 二进制大小      | 28 MB + Node.js | 3.4 MB   | ~8 MB    | **~25 MB**（基础）/ **~36 MB**（含 OTel） |
| Docker 镜像     | —               | —        | —        | **~50 MB**（Alpine）                    |
| 内存占用（空闲）| > 1 GB          | < 5 MB   | < 10 MB  | **~35 MB**                              |
| 启动时间        | > 5 s           | < 10 ms  | < 1 s    | **< 1 s**                               |
| 目标硬件        | $599+ Mac Mini  | $10 边缘设备 | $10 边缘设备 | **$5 VPS+**                         |

| 功能特性                   | OpenClaw                             | ZeroClaw                                     | PicoClaw                              | **GoClaw**                     |
| -------------------------- | ------------------------------------ | -------------------------------------------- | ------------------------------------- | ------------------------------ |
| 多租户（PostgreSQL）       | —                                    | —                                            | —                                     | ✅                             |
| MCP 集成                   | —（使用 ACP）                        | —                                            | —                                     | ✅（stdio/SSE/streamable-http）|
| 智能体团队                 | —                                    | —                                            | —                                     | ✅ 任务板 + 邮箱               |
| 安全加固                   | ✅（SSRF、路径遍历、注入）           | ✅（沙箱、速率限制、注入、配对）             | 基础（工作区限制、exec 拒绝）         | ✅ 5 层防御                    |
| OTel 可观测性              | ✅（可选扩展）                       | ✅（Prometheus + OTLP）                      | —                                     | ✅ OTLP（可选构建标签）        |
| 提示词缓存                 | —                                    | —                                            | —                                     | ✅ Anthropic + OpenAI 兼容     |
| 知识图谱                   | —                                    | —                                            | —                                     | ✅ 大语言模型提取 + 遍历       |
| 技能系统                   | ✅ 嵌入/语义                         | ✅ SKILL.md + TOML                           | ✅ 基础                               | ✅ BM25 + pgvector 混合        |
| 基于通道的调度器           | ✅                                   | 有界并发                                     | —                                     | ✅（main/subagent/team/cron）  |
| 消息渠道                   | 37+                                  | 15+                                          | 10+                                   | 7+                             |
| 伴侣应用                   | macOS、iOS、Android                  | Python SDK                                   | —                                     | Web 控制台                     |
| 实时画布 / 语音            | ✅（A2UI + TTS/STT）                 | —                                            | 语音转录                              | TTS（4 个提供商）              |
| 大语言模型提供商           | 10+                                  | 8 原生 + 29 兼容                             | 13+                                   | **20+**                        |
| 每用户工作空间             | ✅（基于文件）                       | —                                            | —                                     | ✅（PostgreSQL）               |
| 加密密钥                   | —（仅环境变量）                      | ✅ ChaCha20-Poly1305                         | —（明文 JSON）                        | ✅ 数据库中 AES-256-GCM        |

## 桌面版（GoClaw Lite）

面向本地 AI 智能体的原生桌面应用 —— 不需要 Docker，不需要 PostgreSQL，不需要任何基础设施。

**macOS：**
```bash
curl -fsSL https://raw.githubusercontent.com/nextlevelbuilder/goclaw/main/scripts/install-lite.sh | bash
```

**Windows（PowerShell）：**
```powershell
irm https://raw.githubusercontent.com/nextlevelbuilder/goclaw/main/scripts/install-lite.ps1 | iex
```

### 包含内容
- 单个原生应用（Wails v2 + React），约 30 MB
- SQLite 数据库（零配置）
- 与智能体对话（流式输出、工具调用、媒体、文件附件）
- 智能体管理（最多 5 个）、提供商配置、MCP 服务器、技能、定时任务
- 带看板视图和实时更新的团队任务
- 从 GitHub Releases 自动更新

### Lite 与 Standard 对比

| 功能 | Lite（桌面版） | Standard（服务端） |
|---------|---------------|-------------------|
| 智能体 | 最多 5 个 | 不限 |
| 团队 | 最多 1 个（5 名成员） | 不限 |
| 数据库 | SQLite（本地） | PostgreSQL |
| 记忆 | FTS5 全文检索 | pgvector 语义检索 |
| 渠道 | — | Telegram、Discord、Slack、Zalo、飞书、WhatsApp |
| 知识图谱 | — | 完整 |
| RBAC / 多租户 | — | 完整 |
| 自动更新 | GitHub Releases | Docker / 二进制 |

### 从源码构建
```bash
# 前置条件：Go 1.26+、pnpm、Wails CLI（go install github.com/wailsapp/wails/v2/cmd/wails@latest）
make desktop-build                    # 构建 .app（macOS）或 .exe（Windows）
make desktop-dmg VERSION=0.1.0        # 生成 .dmg 安装包（仅 macOS）
make desktop-dev                      # 开发模式，支持热重载
```

### 桌面版发布
桌面版使用独立版本号，以 `lite-v*` 打标签：
```bash
git tag lite-v0.1.0 && git push origin lite-v0.1.0
# → GitHub Actions 构建 macOS（.dmg + .tar.gz）+ Windows（.zip）
# → 创建包含全部产物的 GitHub Release
```
## 架构

<p align="center">
  <img src="_statics/Multi-Tenant Architecture.jpg" alt="多租户架构" width="800" />
</p>

<p align="center">
  <img src="_statics/3-Tier Memory Architecture.jpg" alt="3 层记忆" width="800" />
</p>

<p align="center">
  <img src="_statics/8-Stage Agent Pipeline.jpg" alt="8 阶段智能体流水线" width="800" />
</p>

<p align="center">
  <img src="_statics/Mode Prompt System.jpg" alt="4 种提示词模式" width="800" />
</p>

## 快速开始

**前置条件：** Go 1.26+、PostgreSQL 18（含 pgvector）、Docker（可选）

### 从源码构建

```bash
git clone -b main https://github.com/nextlevelbuilder/goclaw.git && cd goclaw
make build
./goclaw onboard        # 交互式设置向导
source .env.local && ./goclaw
```

> **注意：** 默认分支是 `dev`（活跃开发中）。请使用 `-b main` 克隆稳定发布分支。

### 使用 Docker

```bash
# 生成带有自动密钥的 .env 文件
chmod +x prepare-env.sh && ./prepare-env.sh

# 在 .env 中至少添加一个 GOCLAW_*_API_KEY，然后：
make up

# 如果 Postgres 启动失败（"port 5432 already allocated"），在 .env 中换一个宿主端口，
# 例如 POSTGRES_PORT=5433（参见 .env.example）。

# Web 控制台：http://localhost:18790（内置）
# 健康检查：curl http://localhost:18790/health

# 可选：使用独立 nginx 承载自定义 SSL / 反向代理
# make up WITH_WEB_NGINX=1  → 控制台在 http://localhost:3000
```

`make up` 会创建 Docker 网络、从 git 标签嵌入正确版本、构建并启动全部服务，并自动执行数据库迁移。

**常用命令：**

```bash
make up                # 启动全部服务（构建 + 迁移）
make down              # 停止全部服务
make logs              # 跟踪日志（goclaw 服务）
make reset             # 清空数据卷并从头重建
```

**运维 CLI：**

主程序 `goclaw` 也可以检查本地或远程网关：

```bash
goclaw traces list --status error
goclaw traces get <trace-id> -o json
goclaw --server https://goclaw.example.com --token "$GOCLAW_GATEWAY_TOKEN" traces follow --session <session-key>
```

**可选服务** —— 使用 `WITH_*` 开关启用：

| 开关 | 服务 | 作用 |
|------|---------|-------------|
| `WITH_BROWSER=1` | 无头 Chrome | 启用 `browser` 工具，用于网页抓取、截图、自动化 |
| `WITH_OTEL=1` | Jaeger | OpenTelemetry 追踪界面，用于调试大语言模型调用与延迟 |
| `WITH_SANDBOX=1` | Docker 沙箱 | 为运行智能体产生的不可信代码提供隔离容器 |
| `WITH_TAILSCALE=1` | Tailscale | 通过 Tailscale 私有网络暴露网关 |
| `WITH_REDIS=1` | Redis | 基于 Redis 的缓存层 |

开关可以组合，并对所有命令生效：

```bash
# 同时启用浏览器自动化与追踪
make up WITH_BROWSER=1 WITH_OTEL=1

# 停止全部服务，包括可选服务
make down WITH_BROWSER=1 WITH_OTEL=1
```

当设置了 `GOCLAW_*_API_KEY` 环境变量时，网关会自动完成初始化，无需交互提示 —— 自动检测提供商、运行数据库迁移并填充默认数据。

> **Docker 镜像变体：**
> | 镜像 | 说明 |
> |-------|-------------|
> | `latest` | 后端 + 内嵌 Web 界面 + Python（**推荐**） |
> | `latest-base` | 仅后端 API，无 Web 界面、无运行时、无媒体探测工具（缺少 `ffprobe` 时 `read_video` 与 `read_audio` 会拒绝执行；缺少 `pdfinfo` 时 `read_document` 会退回到 1000 页上限） |
> | `latest-full` | 全部运行时 + 技能依赖预装 |
> | `latest-otel` | Latest + OpenTelemetry 追踪 |
> | `goclaw-web` | 独立 nginx + React SPA（用于自定义反向代理） |
>
> 自定义构建（Tailscale、Redis）：`docker build --build-arg ENABLE_TSNET=true ...`
> 详见[部署指南](https://docs.goclaw.sh/#deploy-docker-compose)。

## Easypanel

[Easypanel](https://easypanel.io) 是一个自托管的 Docker 部署平台，GoClaw 在上面提供了一键部署模板：https://easypanel.io/templates/goclaw。它会部署完整技术栈（含 pgvector 的 Postgres、GoClaw 后端、用于浏览器自动化的无头 Chrome，以及 Web 界面）—— 部署完成后你仍然需要自行设置 `GOCLAW_*_API_KEY`。

## 更新

### Docker
```bash
docker compose pull && docker compose up -d
```

### 二进制（内嵌 Web 界面）
```bash
goclaw update --apply    # 下载、校验 SHA256、替换二进制、重启
```

### Web 控制台
打开 **关于** 对话框 → 点击 **立即更新**（仅管理员）。使用默认 `latest` 镜像时，更新会同时覆盖后端和 Web 控制台。
## 多智能体编排

<p align="center">
  <img src="_statics/Agent Orchestration.jpg" alt="智能体编排" width="800" />
</p>

每个智能体都以自身的身份、工具、大语言模型提供商和上下文文件运行。
Agent Links 定义出站、入站或双向的权限边。
委托可以同步或异步执行，并通过一个隔离的委托工作区交换文件；校验通过的产出会发布回
调用方的 `.delegations/<delegation-id>/` 目录。

### 智能体委托

| 模式 | 工作方式 | 适用场景 |
|------|----------|----------|
| **同步** | 智能体 A 向智能体 B 发起请求并**等待**回答 | 快速查询、事实核查 |
| **异步** | 智能体 A 向智能体 B 发起请求后**继续执行**，B 完成后再通知 | 长时间任务、报告生成、深度分析 |

智能体通过明确的**权限链接**进行通信，支持方向控制（`outbound`、`inbound`、`bidirectional`）
以及链接级和智能体级的并发限制。

### 智能体团队

- **共享任务板** — 创建、认领、完成、搜索任务，支持 `blocked_by` 依赖关系
- **团队邮箱** — 点对点直接消息和广播
- **工具**：`team_tasks` 用于任务管理，`team_message` 用于邮箱

> 详情参见[智能体团队文档](https://docs.goclaw.sh/#teams-what-are-teams)。

## 知识仓库

<p align="center">
  <img src="_statics/Knowledge Vault.jpg" alt="知识仓库" width="800" />
</p>

带 `[[wikilinks]]` 双向链接的文档注册表。混合检索将全文（BM25）与语义（pgvector）结合以实现
精准召回。文件系统同步让仓库内容与磁盘上的文件保持一致。

## 自我演化

<p align="center">
  <img src="_statics/Self-Evolution System.jpg" alt="自我演化" width="800" />
</p>

智能体通过一条带护栏的 3 阶段流水线改进自身：指标采集 → 建议分析 → 自动适配。它可以打磨
沟通风格与领域能力（CAPABILITIES.md），但**绝不**改变身份、名称或核心目标。

## 提供商适配器

<p align="center">
  <img src="_statics/Provider Adapter System.jpg" alt="提供商适配器" width="800" />
</p>

20 多个大语言模型提供商通过统一的适配器接口接入。基于能力的路由、API 密钥加密
（AES-256-GCM）、按提供商的 extended thinking 支持，以及 Anthropic + OpenAI 的提示词缓存。

## 事件驱动架构

<p align="center">
  <img src="_statics/DomainEventBus.jpg" alt="DomainEventBus" width="800" />
</p>

类型化的领域事件驱动整条记忆固化流水线 —— 会话摘要、知识图谱抽取、dreaming 提升，全部通过
带带去重与重试的工作池异步执行。
## 内置工具

8 个分组，30 多个工具：

| 分组 | 工具 | 说明 |
|----------|-------|-------------|
| **文件系统** | `read_file`、`write_file`、`edit_file`、`list_files`、`search`、`glob` | 支持虚拟文件系统路由的文件操作 |
| **运行时** | `exec`、`browser` | Shell 命令（含审批流程）+ 浏览器自动化 |
| **网络** | `web_search`、`web_fetch` | 搜索（Brave、DuckDuckGo）+ 内容抽取 |
| **记忆** | `memory_search`、`memory_get`、`knowledge_graph_search` | 3 层记忆 + 知识图谱遍历 |
| **媒体** | `create_image`、`create_audio`、`create_video`、`read_*`、`tts` | 生成 + 分析（多提供商） |
| **技能** | `skill_search`、`use_skill`、`skill_manage` | BM25 + 语义混合检索 |
| **团队** | `team_tasks`、`spawn`、`delegate`、`message` | 任务板 + 编排 + 消息 |
| **自动化** | `cron`、`heartbeat`、`sessions_*` | 调度 + 会话管理 |

明细清单：

| 工具               | 分组          | 描述                                                         |
| ------------------ | ------------- | ------------------------------------------------------------ |
| `read_file`        | fs            | 读取文件内容（支持虚拟文件系统路由）                         |
| `write_file`       | fs            | 写入/创建文件                                                |
| `edit_file`        | fs            | 对现有文件进行精确编辑                                       |
| `list_files`       | fs            | 列出目录内容                                                 |
| `search`           | fs            | 按模式搜索文件内容                                           |
| `glob`             | fs            | 按 glob 模式查找文件                                         |
| `exec`             | runtime       | 执行 Shell 命令（含审批流程）                                |
| `browser`          | ui            | 浏览器自动化（导航、点击、输入、截图）                       |
| `web_search`       | web           | 网络搜索（Brave、DuckDuckGo）                                |
| `web_fetch`        | web           | 抓取并解析网页内容                                           |
| `memory_search`    | memory        | 搜索长期记忆（全文检索 + 向量）                              |
| `memory_get`       | memory        | 获取记忆条目                                                 |
| `knowledge_graph_search` | memory  | 搜索实体并遍历知识图谱关系                                   |
| `skill_search`     | —             | 搜索技能（BM25 + 嵌入混合）                                  |
| `create_image`     | media         | 图像生成（DashScope、MiniMax）                               |
| `create_audio`     | media         | 音频生成（OpenAI、ElevenLabs、MiniMax、Suno）                |
| `create_video`     | media         | 视频生成（MiniMax、Veo）                                     |
| `read_document`    | media         | 文档读取（Gemini File API、提供商链）                        |
| `read_image`       | media         | 图像分析                                                     |
| `read_audio`       | media         | 音频转录与分析                                               |
| `read_video`       | media         | 视频分析                                                     |
| `tts`              | —             | 文字转语音合成                                               |
| `message`          | messaging     | 向渠道发送消息                                               |
| `spawn`            | —             | 生成子智能体                                                 |
| `subagents`        | sessions      | 控制运行中的子智能体                                         |
| `team_tasks`       | teams         | 共享任务板（列出、创建、认领、完成、搜索）                   |
| `team_message`     | teams         | 团队邮箱（发送、广播、读取）                                 |
| `sessions_list`    | sessions      | 列出活跃会话                                                 |
| `sessions_history` | sessions      | 查看会话历史                                                 |
| `sessions_send`    | sessions      | 向会话发送消息                                               |
| `sessions_spawn`   | sessions      | 生成新会话                                                   |
| `session_status`   | sessions      | 检查会话状态                                                 |
| `cron`             | automation    | 调度和管理定时任务                                           |
| `gateway`          | automation    | 网关管理                                                     |
| `announce_queue`   | automation    | 异步结果通知（用于异步委托）                                 |

> 完整工具参考见 [docs.goclaw.sh](https://docs.goclaw.sh/#custom-tools)

## Webhook API

无需网关令牌，即可从外部系统触发智能体或发送渠道消息。

```bash
# Bearer 认证 —— 同步大语言模型调用
curl -X POST https://example.com/v1/webhooks/llm \
  -H "Authorization: Bearer wh_..." \
  -H "Content-Type: application/json" \
  -d '{"input":"Summarize today metrics","mode":"sync"}'

# HMAC 认证 —— 使用 create 响应中的 hmac_signing_key 签名
TS=$(date +%s); BODY='{"input":"hi","mode":"sync"}'
SIG=$(echo -n "${TS}.${BODY}" | openssl dgst -sha256 -mac HMAC \
      -macopt "hexkey:${WEBHOOK_HMAC_KEY}" | awk '{print $2}')
curl -X POST https://example.com/v1/webhooks/llm \
  -H "Content-Type: application/json" \
  -H "X-Webhook-Id: ${WEBHOOK_ID}" \
  -H "X-GoClaw-Signature: t=${TS},v1=${SIG}" \
  -d "$BODY"
```

完整参考见 **[docs/webhooks.md](docs/webhooks.md)**：认证、异步回调、重试计划、HMAC 示例、
渠道支持矩阵。
## 文档

完整文档请访问 **[docs.goclaw.sh](https://docs.goclaw.sh)** —— 或在 [`goclaw-docs/`](https://github.com/nextlevelbuilder/goclaw-docs) 中浏览源码。

| 章节 | 主题 |
|------|------|
| [快速上手](https://docs.goclaw.sh/#what-is-goclaw) | 安装、快速启动、配置、Web 控制台导览 |
| [核心概念](https://docs.goclaw.sh/#how-goclaw-works) | 智能体循环、会话、工具、记忆、多租户 |
| [智能体](https://docs.goclaw.sh/#creating-agents) | 创建智能体、上下文文件、人格设定、共享与访问 |
| [提供商](https://docs.goclaw.sh/#providers-overview) | Anthropic、OpenAI、OpenRouter、Gemini、DeepSeek，以及 15+ 个其他提供商 |
| [渠道](https://docs.goclaw.sh/#channels-overview) | Telegram、Discord、Slack、飞书、Zalo、WhatsApp、WebSocket |
| [智能体团队](https://docs.goclaw.sh/#teams-what-are-teams) | 团队、任务板、消息传递、委托与交接 |
| [高级功能](https://docs.goclaw.sh/#custom-tools) | 自定义工具、MCP、技能、定时任务、沙箱、钩子、RBAC |
| [部署](https://docs.goclaw.sh/#deploy-docker-compose) | Docker Compose、数据库、安全、可观测性、Tailscale |
| [参考](https://docs.goclaw.sh/#cli-commands) | CLI 命令、REST API、WebSocket 协议、环境变量 |

## 测试

```bash
go test ./...                                    # 单元测试
go test -v ./tests/integration/ -timeout 120s    # 集成测试（需要正在运行的网关）
```

## 项目状态

有关详细功能状态（包括已在生产环境验证的内容和仍在进行中的内容），请参阅 [CHANGELOG.md](CHANGELOG.md)。

## 致谢

GoClaw 最初的架构灵感来自 [OpenClaw](https://github.com/openclaw/openclaw) 项目，并基于它完成
Go 移植，具备增强的安全性、多租户 PostgreSQL 支持与生产级可观测性。我们对启发这一移植版本的
架构设计和愿景深表感谢。

## 许可证

[CC BY-NC 4.0](LICENSE) — Creative Commons Attribution-NonCommercial 4.0 International

你可以自由复制、修改并再分发本代码，但必须署名、标注你所做的修改，且**不得用于商业目的**。
注意这是一份「源码可见」许可证，并非 OSI 认证的开源协议；商标与专利不在授权范围内。

## Star History

<a href="https://star-history.dera.page/#nextlevelbuilder/goclaw&type=date&legend=top-left">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://star-history.dera.page/svg?repos=nextlevelbuilder/goclaw&type=date&theme=dark&legend=top-left" />
   <source media="(prefers-color-scheme: light)" srcset="https://star-history.dera.page/svg?repos=nextlevelbuilder/goclaw&type=date&legend=top-left" />
   <img alt="Star History Chart" src="https://star-history.dera.page/svg?repos=nextlevelbuilder/goclaw&type=date&legend=top-left" />
 </picture>
</a>





