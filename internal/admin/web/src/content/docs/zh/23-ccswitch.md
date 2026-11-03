---
slug: ccswitch
title: CC Switch 配置管理
group: 辅助工具
order: 23
intro: 使用 CC Switch 管理 Codex Desktop App 的 HypiToken 配置，也支持 Claude Code。
---

## 一、下载安装

官方网站：**[https://ccswitch.io/](https://ccswitch.io/)**。下载适合系统的安装包，安装并启动 CC Switch。

本教程默认使用原生 Windows 的 Codex Desktop App，无需先安装 Claude Code。CC Switch 管理配置，Codex Desktop App 执行任务。

## 二、导入 Codex 配置

1. 安装 Codex Desktop App，并打开 CC Switch。
2. 在 HypiToken [密钥管理](/app/tokens) 点击 **使用令牌**，默认选中 **Codex**。
3. 点击 **在 CC Switch 中打开**，确认导入后，在 CC Switch 的 Codex 配置中启用 HypiToken。
4. 完全退出 Codex Desktop App，重新打开并新建会话。

| 配置项 | 值 |
| --- | --- |
| 名称 | HypiToken |
| Base URL | `https://api.novadiffusion.com/v1` |
| API Key | 你的 HypiToken 令牌 |
| Windows 配置目录 | `%USERPROFILE%\.codex` |

完整步骤见 [Codex Desktop App 教程](/docs/codex-cli)。切换后检查 HypiToken 请求记录，确认实际走向。

## 三、Claude Code（可选）

需要 Claude Code 时，先按 [Claude Code 教程](/docs/claude-code) 安装，再切换「使用令牌」弹窗中的 Claude Code 标签导入。它使用 `https://api.novadiffusion.com`，不带 `/v1`，不要与 Codex 的地址混用。

## 四、常见问题

- **点击没反应**：先启动 CC Switch，再重试；或复制导入链接到 CC Switch。
- **配置没生效**：确认启用了正确服务商，完全退出并重开桌面端。
- **找不到配置**：在文件资源管理器地址栏输入 `%USERPROFILE%\.codex`；设置过 `CODEX_HOME` 时检查实际目录。
- **Work browser 认证提示**：保留 ChatGPT 登录，按 [桌面端教程](/docs/codex-cli) 排查。

终端和 WSL 用户请看单独的 [Codex CLI 教程](/docs/codex-cli-terminal)。
