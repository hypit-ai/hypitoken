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

## 一、推荐：通过 CC Switch 路由模式接入

本教程统一推荐 **路由模式**。一键导入只添加服务商，**不等于已经开启路由**。

1. 从 [CC Switch 官网](https://ccswitch.io/) 安装当前稳定版。2026-11-03 核对的最新版为 [v4.0.25](https://github.com/farion1231/cc-switch/releases/tag/v4.0.25)。
2. 在 HypiToken [密钥管理](/app/tokens) 点击 **使用令牌**，确认主模型并点击 **在 CC Switch 中打开**，确认导入 HypiToken。
3. 若桌面端没有 ChatGPT 登录，先在 CC Switch 切到 **OpenAI Official**，完全退出并重新打开 Codex，完成官方登录，然后再次完全退出 Codex。
4. 在 CC Switch 的 **Codex → 路由** 页点击 **开始路由**，在 HypiToken 卡片选择 **路由到这里**，确认「路由」显示 **生效中**。只点击顶部标签不会改变连接模式。
5. 重开 Codex Desktop。优先继续原会话；若报模型或历史兼容错误，选择 HypiToken 支持的模型并新建会话，保留原会话记录。
6. 使用过程中保持 CC Switch 在后台运行。以后切换服务商，留在路由页选择目标，不要来回切到直连或聚合模式。

操作依据：[CC Switch 官方切换指南](https://github.com/farion1231/cc-switch/blob/main/docs/user-manual/zh/2-providers/2.2-switch.md)及[维护者的登录恢复建议](https://github.com/farion1231/cc-switch/issues/8431)。

导入后检查这些信息：

| 配置项 | 值 |
| --- | --- |
| 客户端 | Codex |
| 服务商 | HypiToken |
| CC Switch 服务商地址 | `https://api.novadiffusion.com/v1` |
| Codex 生效中的连接模式 | 路由 |
| API Key | 你刚创建的 HypiToken 令牌 |
| 接口 | Responses |

点击后没反应时，先打开 CC Switch，再回到页面重试；也可以复制导入链接，在 CC Switch 中导入。下载入口统一使用 [https://ccswitch.io/](https://ccswitch.io/)。

## 二、原生 Windows 配置位置

在文件资源管理器的地址栏输入：

```text
%USERPROFILE%\.codex
```

桌面端的用户配置文件是该目录下的 `config.toml`。先备份已有配置，再用文本编辑器修改；不要重复添加同名设置或表。若你设置过 `CODEX_HOME`，应编辑那个目录里的 `config.toml`。

开启路由后，Codex 的 `config.toml` 指向 CC Switch 本机地址是正常的；真实 HypiToken 地址和令牌保存在 CC Switch 的服务商配置里，不要把本机地址手改回网关地址。由 CC Switch 管理配置和相应凭据。确认主模型为你选择的模型、服务商已启用、接口地址以 `/v1` 结尾，再重启桌面端。不要同时套用另一份环境变量配置，也不要直接覆盖已有的 `auth.json`；需要 Work 功能时，保留官方登录。

macOS 配置目录为 `~/.codex`。桌面端用户使用 CC Switch 即可，手动终端配置请看单独的 [Codex CLI 教程](/docs/codex-cli-terminal)。

## 三、切换后旧会话不能用怎么办

| 表现 | 已知原因与处理 |
| --- | --- |
| `Work browser access requires an authenticated account` | 直连切第三方时可能暂存官方登录。按上面的步骤恢复登录并开启路由；v4.0.16 已修复退出/重启 CC Switch 丢登录的问题。继续直连可尝试「非接管切换时保留官方登录」，但不是本教程默认方案。[#8431](https://github.com/farion1231/cc-switch/issues/8431) |
| `Model provider ... not found` | 历史会话引用的 provider 定义被切换移除。新版有统一 provider / 保留旧定义的处理，但能打开历史不代表能换一家继续发请求。官方与第三方之间续聊可尝试「设置 → 应用配置 → Codex → 统一 Codex 会话历史」，并切换到目标供应商支持的模型；旧推理密文不能跨供应商解密时仍需新会话。不要自行改 provider ID 或删除历史。[#7910](https://github.com/farion1231/cc-switch/issues/7910) |
| 聚合切直连后，旧会话出现 `ccs-` 模型或压缩 400 | 旧模型标识及第三方历史结构可能无法被官方后端接受。维护者表示直连不经过 CC Switch，无法自动修复这类历史；建议新建会话，不要自动改写历史文件。[#8655](https://github.com/farion1231/cc-switch/issues/8655) |

**路由模式不能保证任意旧会话跨供应商续聊。** 新服务商不支持旧模型、上下文或工具格式时，仍可能需要新会话。它主要减少反复改写连接配置和官方登录带来的问题。

### 官方额度耗尽的例外

保留官方登录时，部分桌面版本可能在请求发出前因官方额度耗尽进入 Reserve，导致第三方模型也发不出去。**这不代表 HypiToken 余额耗尽。**

v4.0.25 增加「设置 → 应用配置 → Codex 应用增强 → 官方额度保护」，默认关闭；可提前启用，尽量避免经本机路由的官方请求把额度用尽。但它不处理其他设备消耗，也不能解除已经发生的锁定。已被锁定时，按维护者建议临时用第三方直连、关闭直连保留官方登录并重启后新建会话，或使用 CLI；官方额度恢复后再回到路由。维护者尚未对额度实际耗尽的完整流程完成验证。[#7962](https://github.com/farion1231/cc-switch/issues/7962)

## 四、确认请求走 HypiToken

正常使用时，对照 CC Switch 路由记录和 HypiToken 请求日志的时间、模型及令牌。导入成功或官方登录恢复，不等于模型请求已经走 HypiToken。

- 只有 `127.0.0.1` 连接错误：确认 CC Switch 正在运行、路由已生效。
- 401：检查 CC Switch 服务商卡片中的令牌。
- 404 / 模型不支持：检查服务商地址末尾的 `/v1` 和所选模型。

## 五、终端用户

[Codex CLI 教程](/docs/codex-cli-terminal) 单独介绍终端、WSL 和自动化，桌面端用户无需安装 WSL。
