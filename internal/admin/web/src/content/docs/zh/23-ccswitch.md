---
slug: ccswitch
title: CC Switch 配置管理
group: 辅助工具
order: 23
intro: 推荐通过 CC Switch 路由模式接入 Codex Desktop App，保留官方登录并减少切换配置带来的旧会话问题。
---

## 一、下载安装

官方网站：**[https://ccswitch.io/](https://ccswitch.io/)**。下载适合系统的安装包，安装并启动 CC Switch。

本教程默认使用原生 Windows 的 Codex Desktop App，无需先安装 Claude Code。CC Switch 管理配置，Codex Desktop App 执行任务。

## 二、导入 Codex 配置

1. 安装 Codex Desktop App，并打开 CC Switch。
2. 在 HypiToken [密钥管理](/app/tokens) 点击 **使用令牌**，默认选中 **Codex**。
3. 点击 **在 CC Switch 中打开** 并确认导入；导入本身不会开启路由。
4. 按[桌面教程](/docs/codex-cli)保留或恢复 ChatGPT 登录，完全退出 Codex。
5. 进入 **Codex → 路由**，点击 **开始路由**，选择 HypiToken **路由到这里**，确认「生效中」。
6. 重开 Codex，并保持 CC Switch 在后台运行。以后优先在路由页切换目标。

| 配置项 | 值 |
| --- | --- |
| 名称 | HypiToken |
| Base URL | `https://api.novadiffusion.com/v1` |
| API Key | 你的 HypiToken 令牌 |
| Windows 配置目录 | `%USERPROFILE%\.codex` |

完整步骤见 [Codex Desktop App 教程](/docs/codex-cli)。切换后检查 HypiToken 请求记录，确认实际走向。

## 三、Claude Code（可选）

需要 Claude Code 时，先按 [Claude Code 教程](/docs/claude-code) 安装，再切换「使用令牌」弹窗中的 Claude Code 标签导入。它使用 `https://api.novadiffusion.com`，不带 `/v1`，不要与 Codex 的地址混用。

## 四、旧会话与连接模式

桌面端默认推荐路由模式。直连切换会改写客户端连接配置，部分旧会话会因登录丢失、provider 定义或历史模型不兼容而不能继续；路由减少这类切换，但不保证任意会话跨供应商兼容。已有问题、版本修复和官方额度耗尽时的例外，见[桌面端排障](/docs/codex-cli)。不要为了恢复会话直接删除 `auth.json` 或历史文件。

## 五、常见问题

- **点击没反应**：先启动 CC Switch，再重试；或复制导入链接到 CC Switch。
- **配置没生效**：确认启用了正确服务商，完全退出并重开桌面端。
- **找不到配置**：在文件资源管理器地址栏输入 `%USERPROFILE%\.codex`；设置过 `CODEX_HOME` 时检查实际目录。
- **Work browser 认证提示**：保留 ChatGPT 登录，按 [桌面端教程](/docs/codex-cli) 排查。

终端和 WSL 用户请看单独的 [Codex CLI 教程](/docs/codex-cli-terminal)。
