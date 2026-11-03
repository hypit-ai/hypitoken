---
slug: codex-cli
title: Codex Desktop App 接入
group: 客户端接入
order: 20
intro: 以原生 Windows 的 Codex Desktop App 为例，使用 HypiToken 令牌配置桌面端。推荐通过 CC Switch 导入。
---

## 开始之前

- 安装 Codex Desktop App；已有桌面端就直接继续，无需安装 Node.js 或 Codex CLI。
- 在 HypiToken [密钥管理](/app/tokens) 创建令牌，并确认账户有可用额度。
- 从 [CC Switch 官网](https://ccswitch.io/) 下载并安装 CC Switch，至少启动一次。

## 一、推荐：通过 CC Switch 导入

1. 在 HypiToken 的密钥管理中点击 **使用令牌**，默认打开 **Codex**。
2. 确认或修改 **主模型**，再点击 **在 CC Switch 中打开**，在 CC Switch 中确认导入 HypiToken。
3. 进入 CC Switch 的 **Codex** 配置，选择并启用刚导入的 HypiToken 服务商。
4. 完全退出 Codex Desktop App（包括托盘中的进程），重新打开应用。
5. 打开项目并新建会话，选择控制台当前支持的模型，例如 `gpt-6-astra`，开始使用。

导入后检查这些信息：

| 配置项 | 值 |
| --- | --- |
| 客户端 | Codex |
| 服务商 | HypiToken |
| Base URL | `https://api.novadiffusion.com/v1` |
| API Key | 你刚创建的 HypiToken 令牌 |
| 接口 | Responses |

点击后没反应时，先打开 CC Switch，再回到页面重试；也可以复制导入链接，在 CC Switch 中导入。下载入口统一使用 [https://ccswitch.io/](https://ccswitch.io/)。

## 二、原生 Windows 配置位置

在文件资源管理器的地址栏输入：

```text
%USERPROFILE%\.codex
```

桌面端的用户配置文件是该目录下的 `config.toml`。先备份已有配置，再用文本编辑器修改；不要重复添加同名设置或表。若你设置过 `CODEX_HOME`，应编辑那个目录里的 `config.toml`。

通过 CC Switch 导入后，由 CC Switch 生成并管理 `config.toml` 和相应凭据。确认主模型为你选择的模型、服务商已启用、接口地址以 `/v1` 结尾，再重启桌面端。不要同时套用另一份环境变量配置，也不要直接覆盖已有的 `auth.json`；需要 Work 功能时，保留官方登录。

macOS 配置目录为 `~/.codex`。桌面端用户使用 CC Switch 即可，手动终端配置请看单独的 [Codex CLI 教程](/docs/codex-cli-terminal)。

## 三、确认接入成功

在桌面端正常发起一个任务，再查看 HypiToken 控制台的请求记录，确认时间、模型和使用的令牌对应。只看到 CC Switch 导入成功，并不代表桌面端已切换服务商。

- **仍走原服务商**：确认已启用 HypiToken，完全退出并重开桌面应用，再新建会话。
- **401**：检查令牌是否完整，以及应用是否读取到凭据。
- **404**：检查 Base URL 末尾是否有 `/v1`。
- **模型不可用**：以控制台模型列表为准，检查模型名称是否一致。
- **Work browser 身份认证提示**：需要编程时选择 Codex 模式；Work 功能可能另需 ChatGPT 登录。保留官方登录，不要把网关令牌当作 Work 登录凭据。

## 四、需要终端版？

[Codex CLI 教程](/docs/codex-cli-terminal) 单独介绍命令行安装、配置、WSL 和自动化用法。使用 Desktop App 的用户无需完成 CLI 教程。
