---
slug: intro
title: 产品介绍
group: 开始使用
order: 1
intro: HypiToken 是一个 OpenAI / Anthropic 双协议兼容的统一 API 网关。一个 Key，在 Codex Desktop App / CLI、Claude Code、Cursor 等官方与主流 AI 编程工具里无缝接入 GPT、Claude 等模型。
---

## 这是什么

HypiToken 是一个 **OpenAI / Anthropic 兼容**的 API 转发服务。把客户端里的 Base URL 改成本站地址、再填入控制台签发的 Key，就能在国内直连主流大模型，**无需任何代理或梯子**。

后端会把你的请求在多个上游订阅凭证之间智能调度（粘性会话 + 故障自动轮换），对你来说体验和官方一致，但更稳定、通常也更便宜。

## 支持的客户端

| 客户端 | 类型 | 协议 | Base URL |
| --- | --- | --- | --- |
| Codex Desktop App / CLI | Desktop / CLI | OpenAI | `https://api.novadiffusion.com/v1` |
| Claude Code | CLI | Anthropic | `https://api.novadiffusion.com` |
| Cursor | GUI | OpenAI | `https://api.novadiffusion.com/v1` |

> **关于 Base URL 末尾的 `/v1`**
>
> Anthropic 协议（Claude Code）**不带** `/v1`；OpenAI 协议（Codex Desktop App / CLI、Cursor）**必须带** `/v1`。这是最常见的 404 报错原因。

> **CCSwitch 不是接入网关的客户端**：它是一个本地凭证 / 配置管理工具，帮你管理 Codex、Claude Code 的服务商配置并一键切换——真正发请求的仍然是 Codex / Claude Code 本身。详见 [CCSwitch 凭证管理](/docs/ccswitch)。

## 哪些客户端不被支持

本站只接受**交互式 AI 编程客户端**。原始 SDK、脚本类调用会在网关入口被拒绝，返回 `403 client not allowed`：

- ❌ 被拦截：OpenAI / Anthropic 官方 SDK、LiteLLM、`curl`、`python-requests`、Postman、各类爬虫脚本
- ✅ 放行：Codex Desktop App / CLI、Claude Code、Cursor、Claude Desktop 等正常交互客户端

也就是说，你**不能**用裸 `curl` 来「测一下接口」——它会被直接拒掉。请用下面的官方客户端接入。

## 整体使用流程

1. **注册账号** — 在 [api.novadiffusion.com](https://api.novadiffusion.com/register) 用邮箱注册。
2. **创建密钥** — 登录控制台，在「密钥管理」中创建并复制密钥（格式 `sk-cpa-...`）。
3. **账户充值** — 新用户有试用额度，长期使用前在「钱包」中充值。
4. **客户端接入** — 选择对应客户端教程，填入 Base URL 和 API Key。

> 第一次接触 API 或命令行？先看 [新手必读](/docs/beginners)，3 分钟搞懂名词 + 终端打开方式。准备好直接上手，看 [快速上手](/docs/quick-start)。
