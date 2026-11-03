---
slug: quick-start
title: 快速上手
group: 开始使用
order: 3
intro: 一个从零开始的最短路径，每一步都对应一个详细子页面。第一次使用 HypiToken 建议按顺序走一遍。
---

## 一条地址，两套协议

| 工具 | Base URL |
| --- | --- |
| Claude Code | `https://api.novadiffusion.com` |
| Codex CLI / Cursor | `https://api.novadiffusion.com/v1` |

同一个 API Key 在这几个客户端通用。网关把 `/v1/chat/*`、`/v1/responses*`、`/v1/models` 路由到 OpenAI 端点，其余请求走 Anthropic 端点。

## 四步打通

### 1. 注册账号 · 1 分钟

用邮箱在 [api.novadiffusion.com/register](https://api.novadiffusion.com/register) 注册。 → [详细步骤](/docs/register)

### 2. 创建密钥 · 30 秒

登录后在「密钥管理」点「创建密钥」，再点旁边的「复制」按钮把密钥复制出来（格式 `sk-cpa-...`）。 → [详细步骤](/docs/create-key)

### 3. 充值额度 · 1 分钟

新用户有试用额度可以先体验。长期使用前在「钱包」中充值。 → [详细步骤](/docs/top-up)

### 4. 选择客户端并接入 · 2~3 分钟

挑你想用的客户端，按对应教程填入 Base URL 和 Key 即可：

- ⚡ [**Codex Desktop App（推荐）**](/docs/codex-cli) — 原生 Windows 桌面端接入
- 🤖 [**Claude Code**](/docs/claude-code) — Anthropic 官方 CLI
- 🎯 [**Cursor**](/docs/cursor) — GUI 编辑器，设置页填几个字段就行

> **想随时切换服务商？** [**CCSwitch**](/docs/ccswitch) 是一个本地凭证 / 配置管理工具，帮你管理 Claude Code、Codex 的服务商配置并一键切换——它本身不是接入网关的客户端，真正发请求的仍是 Claude Code / Codex。

> **一个 Key 走天下**：同一个 API Key 可以同时在这几个客户端使用，不需要为每个工具单独创建密钥。但如果你想分项目统计用量，可以为每个项目建独立 Key。
