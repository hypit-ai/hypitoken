---
slug: codex-cli-terminal
title: Codex CLI 接入
group: 客户端接入
order: 21
intro: OpenAI 官方 Codex CLI 通过 /v1 接入统一网关。同一个 Key 与 Claude Code 通用。
---

> 本页仅用于终端版，包括 WSL。桌面端用户请看 [Codex Desktop App 教程](/docs/codex-cli)。

## 开始之前请确认

- 已注册 HypiToken 账号并创建密钥（[快速上手](/docs/quick-start)）
- 账户有余额或仍有新人试用额度（[如何充值](/docs/top-up)）
- 知道怎么打开命令行（[没用过命令行？点这里](/docs/beginners)）

## 一、关于 Codex CLI

Codex CLI 是 OpenAI 官方出品的终端 AI 编程助手，可以在命令行里用自然语言操控代码、执行命令、读写文件。

| 对比项 | Codex CLI | Claude Code |
| --- | --- | --- |
| 出品方 | OpenAI | Anthropic |
| 接口协议 | OpenAI 格式 | Anthropic 格式 |
| Node 版本要求 | **v22+** | v18+ |
| 环境变量 | `OPENAI_API_KEY` + `OPENAI_BASE_URL` | `ANTHROPIC_AUTH_TOKEN` + `ANTHROPIC_BASE_URL` |

> 网关同时支持两种协议，**一个账号、一个 Key** 可以同时用 Codex 和 Claude Code。

## 二、端点信息

| 项 | 值 |
| --- | --- |
| 协议 | OpenAI Responses / Chat Completions API |
| Base URL | `https://api.novadiffusion.com/v1`（末尾**必须**加 `/v1`） |
| 环境变量 | `OPENAI_API_KEY` + `OPENAI_BASE_URL` |
| API Key | 从控制台「密钥管理」复制，可与 Claude Code 共用 |

> Codex 使用 OpenAI 协议，Base URL 末尾**必须**加 `/v1`，这跟 Claude Code 不同。忘记加会 404。

## 三、安装 Codex CLI

Codex CLI 要求 **Node.js v22+**。

<div data-tabs="install-codex">
<div data-tab="macOS">

```bash
# 用 Homebrew 装 Node 22（或去 nodejs.org 下载 LTS）
brew install node@22

# 安装 Codex CLI
npm install -g @openai/codex
codex --version
```

</div>
<div data-tab="Windows">

OpenAI 当前建议 Windows 用户通过 WSL2 运行 Codex CLI：

```powershell
# 安装 WSL（首次需重启）
wsl --install
```

重启后进入 Ubuntu，安装 Node.js v22 与 Codex CLI：

```bash
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt install -y nodejs
npm install -g @openai/codex
codex --version
```

> 若在原生 PowerShell 遇到 shell / PTY / 权限问题，切到 WSL2 即可。

</div>
<div data-tab="Linux">

```bash
# 安装 Node.js v22
sudo apt update
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt install -y nodejs

# 安装 Codex CLI
npm install -g @openai/codex
codex --version
```

</div>
</div>

## 四、指向网关（推荐：配置文件）

使用 Codex CLI 官方持久化配置文件：`~/.codex/config.toml`（模型与 Base URL）+ `~/.codex/auth.json`（密钥）。

<div data-tabs="config-codex">
<div data-tab="macOS">

```bash
mkdir -p ~/.codex
nano ~/.codex/config.toml
```

填入：

```toml
model_provider = "hypitoken"
model = "gpt-6-astra"
model_reasoning_effort = "high"     # high / medium / low / minimal
disable_response_storage = true     # 第三方网关必须关掉 response 存储

[model_providers.hypitoken]
name = "HypiToken"
base_url = "https://api.novadiffusion.com/v1"
wire_api = "responses"
requires_openai_auth = true
```

再写 `auth.json`：

```bash
cat > ~/.codex/auth.json <<'JSON'
{
  "OPENAI_API_KEY": "YOUR_TOKEN_HERE"
}
JSON
chmod 600 ~/.codex/auth.json
```

</div>
<div data-tab="Windows">

在 WSL2 中（推荐）操作同 macOS / Linux。原生 PowerShell 的配置目录是 `%USERPROFILE%\.codex`：

```powershell
New-Item -ItemType Directory -Force "$env:USERPROFILE\.codex"
notepad "$env:USERPROFILE\.codex\config.toml"
```

`config.toml` 内容：

```toml
model_provider = "hypitoken"
model = "gpt-6-astra"
model_reasoning_effort = "high"     # high / medium / low / minimal
disable_response_storage = true     # 第三方网关必须关掉 response 存储

[model_providers.hypitoken]
name = "HypiToken"
base_url = "https://api.novadiffusion.com/v1"
wire_api = "responses"
requires_openai_auth = true
```

再写 `auth.json`：

```powershell
@'
{
  "OPENAI_API_KEY": "YOUR_TOKEN_HERE"
}
'@ | Set-Content "$env:USERPROFILE\.codex\auth.json"
```

</div>
<div data-tab="Linux">

```bash
mkdir -p ~/.codex
nano ~/.codex/config.toml
```

填入：

```toml
model_provider = "hypitoken"
model = "gpt-6-astra"
model_reasoning_effort = "high"     # high / medium / low / minimal
disable_response_storage = true     # 第三方网关必须关掉 response 存储

[model_providers.hypitoken]
name = "HypiToken"
base_url = "https://api.novadiffusion.com/v1"
wire_api = "responses"
requires_openai_auth = true
```

再写 `auth.json`：

```bash
cat > ~/.codex/auth.json <<'JSON'
{
  "OPENAI_API_KEY": "YOUR_TOKEN_HERE"
}
JSON
chmod 600 ~/.codex/auth.json
```

</div>
</div>

### 四个必填项的作用

| 配置项 | 为什么需要 |
| --- | --- |
| `model` | 默认模型。不写就用 Codex 自带的默认值，可能不在网关的可用集合里。 |
| `model_reasoning_effort` | 推理强度，见下表。 |
| `disable_response_storage` | 必须为 `true`。第三方网关不提供 OpenAI 的 response 存储，留着会让请求失败。 |
| `requires_openai_auth` | 必须为 `true`，否则 Codex 不会把 `auth.json` 里的 Key 发出去，直接 401。 |

`model_reasoning_effort` 取值：

| 值 | 适合的场景 |
| --- | --- |
| `ultra` | 最高推理强度，并自动拆分委派子任务。仅 `gpt-6-astra` 与 `gpt-5.6-sol` 支持。 |
| `max` | 最高推理深度，留给最难的问题。 |
| `xhigh` | 比 `high` 更深一档的推理强度。 |
| `high` | 架构设计、跨文件重构、疑难 bug。较慢也较贵，但成功率最高。 |
| `medium` | 日常写代码、改 bug 的默认选择。 |
| `low` | 简单改动、格式化、写注释，追求响应速度。 |
| `minimal` | 几乎不推理，只做机械转换（翻译、改名、套模板）时最省。 |

> `xhigh`、`max`、`ultra` 只有声明支持的模型才接受——`gpt-6-astra` 支持 `low` 到 `ultra`，更老的档位到 `high` 为止。模型会拒绝它不支持的强度值。

> 推理强度越高，思考 token 越多，计费也越高。拿不准就先用 `medium`。

### 临时调试（环境变量）

<div data-tabs="env-codex">
<div data-tab="macOS">

```bash
export OPENAI_BASE_URL="https://api.novadiffusion.com/v1"
export OPENAI_API_KEY="YOUR_TOKEN_HERE"
```

</div>
<div data-tab="Windows">

WSL2 中：

```bash
export OPENAI_BASE_URL="https://api.novadiffusion.com/v1"
export OPENAI_API_KEY="YOUR_TOKEN_HERE"
```

原生 PowerShell：

```powershell
$env:OPENAI_BASE_URL = "https://api.novadiffusion.com/v1"
$env:OPENAI_API_KEY = "YOUR_TOKEN_HERE"
```

</div>
<div data-tab="Linux">

```bash
export OPENAI_BASE_URL="https://api.novadiffusion.com/v1"
export OPENAI_API_KEY="YOUR_TOKEN_HERE"
```

</div>
</div>

## 五、验证与运行

**检查点 1：Node 是 v22+ 吗？** `node -v` 应输出 `v22` 起步的版本。Codex 对 Node 版本要求较新，v18 / v20 跑不起来。可用 `nvm install 22 && nvm use 22` 切换。

**检查点 2：Codex 装好了吗？** `codex --version` 应输出版本号。

**检查点 3：启动 Codex。** 新开终端执行：

```bash
codex
```

进入交互界面后输入测试：

```text
帮我写一个读取当前目录所有文件名的 Python 脚本
```

## 六、可用模型

OpenAI 侧提供的模型 ID：

```
gpt-6-astra
gpt-5.6-sol  gpt-5.6-terra  gpt-5.6-luna
gpt-5.5  gpt-5.4  gpt-5.4-mini
```

`gpt-6-astra` 是旗舰，复杂、吃力的任务优先用它。`gpt-5.6-sol` 仍是性价比之选——价格是 astra 的一半，同样属于前沿模型。`gpt-5.6-terra` 兼顾能力与成本，`gpt-5.6-luna` 面向高并发的成本敏感场景。

> `gpt-6-astra` 需要 Codex CLI **0.153.0 及以上**。更旧的客户端不会被上游下发该模型，会回落到自带的默认模型。

`GET /v1/models` 返回的是**实际可用集合**（随上游套餐变化），控制台的模型列表为准：

```bash
codex --model gpt-6-astra "帮我优化这段代码"
```

> 模型名可以带后缀并被正确识别计费，例如 `gpt-6-astra(high)`。

## 七、在自动化 / Agent 中使用

`codex exec` 是非交互模式：读入一条任务描述，自动干完后退出，不进 TUI。适合放进脚本、CI、cron 或上层 Agent。

```bash
# 一次性任务，做完即退出
codex exec "把 README 里的安装步骤更新成 Node 22"

# 指定模型
codex exec --model gpt-6-astra "给所有导出函数补 JSDoc"
```

配置照常从 `~/.codex/config.toml` + `~/.codex/auth.json` 读取；在 CI 里没有这两个文件时，直接给环境变量 `OPENAI_BASE_URL` 和 `OPENAI_API_KEY` 即可。

## 八、常见报错排查

**❌ 401 Unauthorized / Invalid API Key** — 检查 Key 是否完整（含 `sk-cpa-` 前缀）、有无空格或换行、余额是否充足。

**❌ 404 Not Found / model does not exist** — Base URL 末尾忘记加 `/v1`，或模型名不对（以控制台为准）。

**❌ `codex: command not found`** — npm 全局 bin 未加入 PATH：

```bash
echo 'export PATH="$(npm config get prefix)/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc
```

**❌ Node.js version too low** — Codex 要求 Node v22+，请升级：`nvm install 22 && nvm use 22`。

**❌ 503 Service Unavailable** — 上游号池暂时没有可用凭证。响应体里写了具体原因，`Retry-After` 头给出建议等待秒数（最长 300 秒）。等一会儿重试即可，或查看[状态页](/status)。

**❌ 402 Payment Required** — 余额不足，去控制台[充值](/docs/top-up)。

对照表：

| 返回码 | 真实原因 |
| --- | --- |
| `401` | Key 缺失/ 写错 / 带了空格换行；或 `requires_openai_auth` 没设成 `true` |
| `402` | 余额不足 |
| `404` | `base_url` 漏了 `/v1`，或模型名不存在 |
| `503` | 上游号池暂时无可用凭证，`Retry-After` 最长 300 秒 |

### Work browser 身份认证报错（桌面端）

如果出现 `Work browser access requires an authentication method`，先按本节检查。公开报告中的相近原文是 `Work browser access requires an authenticated account`，可能伴随发送失败或恢复会话失败；两种措辞尚未在客户设备上核实为同一错误。

**可能原因：** 桌面端 Work 模式初始化浏览器工具时需要 ChatGPT 登录身份，只有网关 API Key 时可能被客户端拦截，模型请求尚未发出。OpenAI 仓库已有对应的 [BYOK / Work 问题报告](https://github.com/openai/codex/issues/54841)。这条提示本身不代表余额不足、Key 无效或 HTTP 429。

#### 只需要编程：先切回 Codex 模式

1. 在桌面应用的模式选择处，从 **Work / ChatGPT Work** 切到 **Codex**，新建会话重试。
2. 若界面无法切换，完全退出应用，按下表找到实际使用的 `config.toml`，先备份再编辑。

| 系统 / 运行环境 | 默认配置文件位置 | 打开方式 |
| --- | --- | --- |
| macOS | `/Users/你的用户名/.codex/config.toml`，即 `~/.codex/config.toml` | Finder 按 `Command + Shift + G`，输入 `~/.codex`，再打开 `config.toml` |
| Linux | 通常为 `/home/你的用户名/.codex/config.toml`，即 `~/.codex/config.toml` | 终端执行 `nano ~/.codex/config.toml` |
| Windows 原生桌面应用 / PowerShell | 通常为 `C:\Users\你的用户名\.codex\config.toml` | 文件资源管理器地址栏输入 `%USERPROFILE%\.codex`；或 PowerShell 执行 `notepad "$env:USERPROFILE\.codex\config.toml"` |
| WSL 内运行的 Codex CLI | WSL 内的 `~/.codex/config.toml`，通常为 `/home/你的WSL用户名/.codex/config.toml` | 在对应的 WSL 发行版终端执行 `nano ~/.codex/config.toml` |

`~` 表示当前运行用户的主目录，实际位置以该用户的主目录为准。Windows 原生与 WSL 默认是两份不同配置：Windows 桌面应用报错时，优先检查 Windows 那份；修改 WSL 配置通常不会影响 Windows 桌面应用。上表列出 WSL 路径用于区分运行环境，CLI 本身不使用桌面端 Work 模式。

如果给 Codex 设置了自定义 `CODEX_HOME`，配置文件位于**该目录下的 `config.toml`**，不再是表中的默认路径。macOS / Linux / WSL 可用 `printenv CODEX_HOME` 查看当前终端的值，PowerShell 可用 `$env:CODEX_HOME`；桌面应用的启动环境可能与终端不同，应以应用实际使用的值为准。不要因为默认路径下找不到文件就覆盖其他配置。

找到已有的 `[desktop]`，修改其中的 `conversationDetailMode`；没有该小节时再添加：

```toml
[desktop]
conversationDetailMode = "STEPS_COMMANDS"
```

不要重复添加 `[desktop]`，不要覆盖整个文件。保留原来的模型、HypiToken 服务商及密钥配置，再打开应用并新建会话测试。这个办法来自用户复现反馈，作用是离开 Work 模式，并非让 API Key 获得 Work 权限；不同版本表现可能不同。

若仍被拦截，可按本页配置使用终端里的 **Codex CLI** 临时继续工作。不要为此删除 `auth.json` 或反复充值。

#### 需要 Work：恢复并保留官方登录

[OpenAI 官方认证说明](https://learn.chatgpt.com/docs/auth)区分 API Key 与 ChatGPT 登录；部分依赖账号或云服务的功能并不随 API Key 提供。请在桌面应用中登录自己的 ChatGPT 账号，并确认对应功能可用。

使用 CC Switch 时，维护者建议先切到 **OpenAI Official** 完成登录，再退出桌面应用，切为**路由模式**并选择 HypiToken 后重开。直连切换则检查「非接管切换时保留官方登录」。具体选项以所装版本为准，见[维护者排查说明](https://github.com/farion1231/cc-switch/issues/8431)。

CC Switch **v4.0.16** 已修复退出路由后丢失官方登录的问题；使用旧版且反复要求登录时，升级到包含该修复的版本。它不取消 Work 的身份要求。[发布说明](https://github.com/farion1231/cc-switch/releases/tag/v4.0.16)

**验证实际走向：** 登录恢复不等于请求仍走网关。发一条短测试消息，对照 CC Switch 路由日志与 HypiToken 控制台请求记录，确认服务商、模型和计费来源。若随后出现真正的 `401`、`402` 或 `429`，应按该响应另外排查。

仍无法解决时，请提供客户端名称与版本、系统、当前模式、是否登录 ChatGPT、CC Switch 版本和路由模式，以及报错时间和脱敏日志；不要发送 API Key、完整 `auth.json` 或登录令牌。以上资料核对日期：2026-10-21；尚未在该客户设备上复现验证。

## 九、直接调用 API

端点同时支持 `/v1/chat/completions` 和 OpenAI 的 `/v1/responses`。发送 OpenAI 请求结构、得到 OpenAI 响应结构，现有工具链无需改动即可工作。

> Codex 端**没有 User-Agent 限制**，curl、官方 openai SDK、LiteLLM 等都能直连。若你要写脚本或跑自动化，请用这两个端点，而不是 Claude 端的 `/v1/messages`（那边有客户端过滤，见 [Claude Code 接入](/docs/claude-code)）。
