# Freebuff2API

[English](README.md) | [简体中文](README_zh.md)

Freebuff2API 是 [Freebuff](https://freebuff.com) 的 OpenAI 兼容代理服务器。本项目将标准 OpenAI API 请求转化为 Freebuff 后端格式，让你能在任何 OpenAI 兼容客户端、SDK 或命令行工具中直接使用 Freebuff 的免费模型。

## 核心特性

- **OpenAI 兼容 API** — 标准 `/v1/chat/completions`、`/v1/models` 与 `/v1/responses`（Responses API）端点。
- **Claude / Anthropic 兼容 API** — 完整的 `/v1/messages` 转换，含流式事件（`message_start` → `content_block_delta` → `message_stop`）、thinking 块、工具调用、图片，以及基于 tiktoken 的 `/v1/messages/count_tokens`。
- **协议兼容层** — 自动补齐免费层要求的 Buffy 系统提示、工具集签名（工具名 `mcp__` 前缀 + `decide` 簽名工具）、harness 提示语改写、强制上游流式并在服务端重组响应、`cb_easp` 停止哨兵等。
- **弹性会话处理** — 免费 Session 生命周期管理，含等待室轮询、`session_superseded` / `session_model_mismatch` / `model_locked` / 空流的自动恢复、基于 `retryAfterMs` 的 429 冷却解析，以及每账号熔断器。
- **稳定账号身份** — 每个 Token 保持稳定的 `client_id`（与官方 SDK 格式一致），而非每次请求随机生成。
- **单账号串行化** — 同一账号的上游请求串行执行并保持最小间隔，匹配免费层单并发行为。
- **多 Token 轮换** — 支持多个认证 Token，自动定期轮换 Run 并平滑排水。
- **动态模型目录** — 解析上游 freebuff 模型常量（`free-agents.ts`、`freebuff-models.ts`、`freebuff-model-ids.ts`），jsDelivr 镜像回退、6 小时刷新、按模型推理强度阶梯，内置快照兜底。
- **健康检查端点** — `/healthz` 汇报每 Token 的会话状态、队列位置、配额档位与剩余额度、健康分类、冷却状态和模型目录状态。
- **HTTP 代理支持** — 可为所有外部请求配置上游 HTTP 代理。
- **Token 获取 CLI** — `cmd/freebuff-token` 完成设备码登录并将 Token 写入 `config.json`。

## 获取 Auth Token

Freebuff2API 需要至少一个 Freebuff **Auth Token**。目前有以下两种获取方式：

### 方式一 — 网页获取（推荐）

访问 **[https://freebuff.llm.pm](https://freebuff.llm.pm)**，使用你的 Freebuff 账号登录后，页面会直接显示你的 Auth Token。复制该值即可作为 **AUTH_TOKENS** 使用，无需在本地安装任何工具。

### 方式二 — Freebuff CLI

安装 Freebuff CLI 并完成登录：

```bash
npm i -g freebuff
```

安装完成后，在终端执行 `freebuff`，首次启动时会自动引导你完成登录。

登录后，Token 会自动保存到本地凭证文件中：

| 系统 | 凭证文件路径 |
|---|---|
| Windows | `C:\Users\<用户名>\.config\manicode\credentials.json` |
| Linux / macOS | `~/.config/manicode/credentials.json` |

文件结构如下：

```json
{
  "default": {
    "id": "user_10293847",
    "name": "张三",
    "email": "zhangsan@example.com",
    "authToken": "fa82b5c1-e39d-4c7a-961f-d2b3c4e5f6a7",
    ...
  }
}
```

将 `authToken` 的值复制出来，即为所需的 **AUTH_TOKENS**。

### 方式三 — 设备码登录 CLI

使用内置 CLI 交互式登录，并直接写入配置文件：

```bash
go run ./cmd/freebuff-token --write-config
```

它会输出登录链接，等待你完成授权，然后将 Token 追加到 `config.json` 的 `AUTH_TOKENS` 中。

> **提示：** 可登录多个账号并配置所有 Token，以提升并发吞吐量。

## 配置指南

支持 JSON 文件和环境变量两种配置方式。JSON 属性名与环境变量名一致。默认在当前目录查找 `config.json`，可通过 `-config` 参数指定其他路径。

```json
{
  "LISTEN_ADDR": ":8080",
  "UPSTREAM_BASE_URL": "https://www.codebuff.com",
  "AUTH_TOKENS": ["token"],
  "ROTATION_INTERVAL": "6h",
  "REQUEST_TIMEOUT": "15m",
  "API_KEYS": [],
  "HTTP_PROXY": "",
  "MAX_REQUEST_BODY_MB": 32,
  "FORCE_UPSTREAM_STREAM": true,
  "TOOL_CAMOUFLAGE": true,
  "HARNESS_REWRITES": true,
  "BUFFY_GUARD": true,
  "UPSTREAM_MIN_GAP": "300ms"
}
```

### 配置参考

| 属性 / 环境变量 | 说明 |
|---|---|
| `LISTEN_ADDR` | 代理监听地址（默认 `:8080`） |
| `UPSTREAM_BASE_URL` | Freebuff 后端地址（默认 `https://www.codebuff.com`） |
| `AUTH_TOKENS` | Freebuff Auth Token（JSON 数组或逗号分隔的环境变量） |
| `ROTATION_INTERVAL` | Run 自动轮换间隔（默认 `6h`） |
| `REQUEST_TIMEOUT` | 上游请求超时时间（默认 `15m`） |
| `API_KEYS` | 客户端鉴权 API Key（留空则无需鉴权） |
| `HTTP_PROXY` | 上游 HTTP 代理地址 |
| `MAX_REQUEST_BODY_MB` | 客户端请求体大小上限（MB，默认 32） |
| `FORCE_UPSTREAM_STREAM` | 强制上游流式并为非流式客户端重组响应（默认 `true`） |
| `TOOL_CAMOUFLAGE` | 上游侧工具名加 `mcp__` 前缀并追加 `decide` 簽名工具（默认 `true`） |
| `HARNESS_REWRITES` | 改写系统提示中的 harness 标识短语（默认 `true`） |
| `BUFFY_GUARD` | 确保系统提示以免费层要求的 Buffy 开头（默认 `true`） |
| `UPSTREAM_MIN_GAP` | 单账号上游请求最小间隔（默认 `300ms`） |

同时设置时，环境变量优先于 JSON 配置文件。

## 部署运行

### Docker 部署

预构建多架构镜像已发布至 GHCR：

```bash
docker run -d --name Freebuff2API \
  -p 8080:8080 \
  -e AUTH_TOKENS="token1,token2" \
  ghcr.io/quorinex/freebuff2api:latest
```

手动构建：

```bash
docker build -t Freebuff2API .
docker run -d -p 8080:8080 -e AUTH_TOKENS="token1,token2" Freebuff2API
```

### 源码编译

**环境要求：** Go 1.23+

```bash
git clone https://github.com/Quorinex/Freebuff2API.git
cd Freebuff2API
go build -o Freebuff2API .
./Freebuff2API -config config.json
```

## 友情链接

- [linux.do](https://linux.do)

## 免责声明

本项目与 OpenAI、Codebuff 或 Freebuff 无任何官方关联，相关商标和版权均归其各自所有者所有。

本仓库的所有内容仅供交流、实验和学习使用，不构成任何生产环境服务或专业建议。本项目按“原样（As-Is）”提供，使用者需自行承担使用风险。作者不对因使用、修改或分发本项目而导致的任何直接或间接损失承担责任，亦不提供任何形式的明示或暗示保证。

## 开源协议

MIT
