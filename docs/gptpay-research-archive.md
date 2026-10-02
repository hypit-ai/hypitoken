# GPTPay 实现与请求复现档案

整理日期：2026-10-01。用户当前不需要 GPTPay 卡付功能，要求从 hypitoken 清除相关运行代码与残留，同时保留实现方案、协议和关键请求复现结果，供以后查阅。本文是历史研究档案，不是启用功能或部署指令。

结论：HTTP 协议实现、浏览器执行层及合成端到端测试均有历史记录。2026-09-19 的独立仓库提交记录进一步报告真实结账的 confirm 请求返回 200，但发卡行拒绝了该次付款。**没有找到完成真实扣款且订单经对账确认 complete 与 paid 的证据，不能记录为实付成功。** 本次只读代码、文档和 Git 历史，没有重新请求上游、使用真实卡片或改变生产。

## 源码位置与版本

- hypitoken 提交 `ee673ce` 已将 GPTPay 提取到独立仓库 `github.com/wjsoj/gptpay`。原始说明可用 `git show ee673ce^:docs/GPTPAY.md` 找回；历史源码可以在该提交的父版本中查阅。
- 独立仓库可放在本项目同级 `../gptpay`。截至整理时 HEAD 为 `44a0603`；前两个相关提交为 `259dd3d`、`f14230b`，均在 2026-09-19。
- 独立仓库当前存在未提交修改，包括 `go.mod`、浏览器 checkout 和部分前端文件；因此不能把整个工作树当成 `44a0603` 的可复现快照。稳定历史结论应按下文标注的提交或文档版本查阅。本次未修改、删除或发布该独立仓库。
- 独立项目拥有 `cmd/gptpay`、`cmd/gptpay-preview`、`internal/gptpay`、`internal/checkout`、`internal/checkout/browser`、`deploy/`；依赖 cc-core 的 auth、网络传输与订阅查询能力。旧文档里的 `cc-core/checkout` 和 hypitoken 内部目录是迁移前路径。
- 协议对照文档引用 `cc-core/crack/chatgpt-checkout/rows/` 的私有抓包编号。本次没有在本机该位置找到对应文件，未读取或复制原始抓包。下文抓包结论来自已保存的对照文档，不能声称本次重验了原始响应。

主要资料：

- [独立项目说明](../../gptpay/README.md)
- [支付协议对照](../../gptpay/internal/checkout/PAYMENT_PARITY.md)
- [订阅查询对照](../../gptpay/internal/checkout/SUBSCRIPTION_PARITY.md)
- [浏览器实现说明](../../gptpay/internal/checkout/BROWSER_AUTOMATION.md)
- [本地浏览器端到端记录](../../gptpay/docs/gptpay-local-browser-e2e-20260916.md)
- [历史生产准备检查](../../gptpay/docs/gptpay-production-readiness-20260916.md)

这些相对链接依赖同级仓库存在；本文保留关键结论，以免只剩失效链接。

## 实现方案

独立 Go 服务内嵌 HTML、CSS、JS 和字体。前端采集用户提供的登录态、套餐、国家、币种、真实账单资料和卡片输入，后端管理本单身份、报价、固定网络出口及付款尝试记录。它不使用 hypitoken 的凭据池或 SaaS 钱包扣费流程。

执行分为两条路线：

1. **HTTP 协议路线**：Go Client 创建 checkout，查询状态和税费，生成 Stripe confirmation token，提交 OpenAI confirm，再提交 PaymentIntent confirm，最后独立对账。它使用抓包整理的私有接口，不是 OpenAI 公开、受支持的支付 API。原始 Go 请求缺少 Stripe.js 和 Elements 的浏览器上下文，字段一致不等于请求等价。
2. **浏览器路线**：Go 通过 chromedp 启动每单独立 Chromium 与隐身上下文，执行页面交互，复用同一浏览器完成报价和查询。9 月 19 日的代码已改为从定价界面选择套餐进入 checkout，替代较早的直接创建请求。卡片 iframe、延迟出现的账单区、页面导航和提交准备状态由浏览器适配层处理。

独立服务历史上由 systemd 管理、通过 Caddy 转发。公开档案只保留架构与协议研究，不包含生产主机、访问入口、配置或数据目录等运维定位信息。2026-10-01 的档案整理没有操作独立生产服务。

历史开关包括 `GPTPAY_ENABLED`、`GPTPAY_BROWSER_ENABLED` 和 `GPTPAY_BROWSER_SUBMIT_ENABLED`。本文保留名称用于辨认历史配置，不在 hypitoken 中恢复任何开关或入口。

## 关键协议与请求顺序

以下仅保存端点与字段结构，不保存可执行的带身份请求，也不保存任何真实订单值。

| 阶段 | 历史请求 | 关键字段及校验 |
| --- | --- | --- |
| 查询订阅 | GET `/backend-api/subscriptions`；GET `/backend-api/accounts/check/v4-2023-04-27` | 合并订阅门户与账号权益；浏览器传入自己的 `timezone_offset_min`。一个接口失败不能把另一接口的有效权益清空 |
| 创建结账 | POST `https://chatgpt.com/backend-api/payments/checkout` | `entry_point`、`plan_name`、`billing_details.country/currency`、`checkout_ui_mode`；响应标识 checkout session 和 processor entity |
| 初始状态 | GET `/backend-api/payments/checkout/{processor_entity}/{checkout_session_id}` | 读取本单状态、套餐及报价相关信息 |
| 计算税费 | POST `/backend-api/payments/checkout/taxes` | `checkout_session_id`、`checkout_email`、`billing_country`、`billing_name`、`currency`、`processor_entity`、嵌套 `billing_address` |
| 生成确认令牌 | POST `https://api.stripe.com/v1/confirmation_tokens` | 抓包由 Stripe.js 与 Elements 生成；不能把 Go form 视为完整 SDK 会话复现 |
| 商户侧确认 | POST `/backend-api/payments/checkout/confirm` | `checkout_session_id`、`confirm_token`、`selected_payment_method_type=card`；响应用于关联本单 PaymentIntent |
| 支付侧确认 | POST `/v1/payment_intents/{payment_intent_id}/confirm` | `return_url`、`confirmation_token`、本单发布密钥、`client_secret`；历史实现固定 `_stripe_version=2025-03-31.basil`，不是对当前版本的建议 |
| 最终对账 | 再次 GET 本单 checkout 状态 | 必须同时 `status=complete`、`payment_status=paid`，并核对账号、套餐、金额、币种和订单关联 |

历史报价实现要求金额为正整数最小货币单位，账号归属来自经上游身份认证的响应；忽略用户自行填写的 Session.user。报价有效期最多 5 分钟且不超过上游 expiry，付款前重新核验。Pro 5x 的 `chatgptprolite` 在早期资料里只是候选套餐代码，不能当成已验证购买映射。

对照资料保存的抓包编号：

| 编号 | 已保存结论 | 证据边界 |
| --- | --- | --- |
| 未捕获 | 创建 checkout 的请求体来自 bookmarklet | 没有原始请求，不能称为完整抓包对齐 |
| 129 | 税费字段与嵌套账单结构匹配 | 仅结构一致 |
| 152 | Stripe confirmation token 使用 SDK、Elements 会话和验证上下文 | Go form 不等价 |
| 174 | OpenAI confirm 的关键字段匹配 | 不证明当前请求被接受 |
| 178 | PaymentIntent confirm 核心字段匹配 | 未完整复现 SDK 会话上下文 |
| 199 | 浏览器成功查询 accounts/check | 未捕获独立的 subscriptions 门户请求 |
| 196 | accounts/optimized/check 使用另一种响应结构 | 不能直接替代 accounts/check |
| 201 | 最终状态含 status 与 payment_status | 成功必须联合判定并绑定本单 |

`/backend-api/subscriptions/auto_top_up/settings?include_payment_method=false` 曾有用户提供的成功浏览器请求。它说明自动充值设置，不说明订阅套餐；`is_enabled=false` 不能推出没有 Plus，`payment_method=null` 也不能推出没有保存卡片。

## 已记录的复现结果

| 时间与来源 | 观察结果 | 可以确认什么 |
| --- | --- | --- |
| 2026-09-15，hypitoken 历史 GPTPAY.md | 格式非法 Session 返回 400；格式合法但虚构的 JWT 在订阅查询被上游拒绝，公开接口返回 502 | 请求分层和错误处理可达，不证明真实订阅数据正确 |
| 同日，传输诊断记录 | uTLS 加服务器直连时，订阅与创建路径均见 403；指定住宅出口后订阅上游变为 401，创建仍 403 | 订阅查询可到业务鉴权层，创建仍受限；不能只凭这些对照断言唯一原因是 IP 或 TLS |
| 同日，SUBSCRIPTION_PARITY.md | 局部查询已得到 account/entitlement，但缺少独立 subscriptions portal 数据 | 可保留账单权益，缺失部分标为 unknown/partial；不能冒充完整订阅查询 |
| 同日，cc-core v0.8.132 相关记录 | 增加浏览器上下文元数据、真实时区与错误脱敏，并部署 | 不代表解决创建或付款 403；当次未做进一步真实凭据验证 |
| 2026-09-16，本地浏览器记录 | 真实 Chromium、真实 HTTP handler、合成上游，验证 paid、pending、金额矛盾、requires_action、验证失败及延迟账单等分支 | 证明本地状态机和对账约束；不是官方实付 |
| 同日，刷新恢复记录 | 合成提交后刷新，敏感输入清空，保留订单编号；重新提供原身份可恢复原浏览器，提交保持锁定 | 该合成场景无新建订单或重复提交；不是完整生产重启验收 |
| 2026-09-19，独立仓库 `f14230b` 提交说明 | 记录真实 checkout 已走完创建、填卡、显示并填写账单，confirm 返回 200，随后发卡行拒绝 | 证据级别是提交作者的历史实测记录，本次无原始响应重验；不能记为 paid |
| 同日，`259dd3d` | 修复第三方 iframe 资源受出口规则阻断，以及 SPA 启动时错过定价 hash | 浏览器兼容性修复，不证明完成扣款 |
| 同日，`44a0603` | 可由付款人在 Stripe 窗口手填 CVC；提交门控仅检查长度而不读取其值 | 是可选手填路径，并不意味着所有输入路径从未接收 CVC |

HTTP 错误应保留操作阶段、状态码及安全分类，不回传上游 HTML 或身份信息。仅见 HTML 不能证明是挑战；历史类型化错误仅在明确 `Cf-Mitigated: challenge` 等证据存在时作对应分类，不能将 200 挑战页误判为业务成功。

## 需要保留的状态约束

- **提交前持久化防重付**：独占创建并 fsync 尝试记录，绑定本单、所有者、账号、套餐、金额和币种。首次可能触发付款的交互之前就写入；结果未知时只查询，不能删记录后重试。
- **延迟账单也是付款边界**：9 月 19 日代码记录，显示账单区的首次交互也可能异步进入 confirm。因此不能把“只是展开表单”当成无扣款风险动作。旧文档“一次原生点击”的表述不完整，应以该提交的实现和风险边界为准。
- **成功必须对账**：HTTP 200、页面文案、导航完成、Stripe processing，甚至单独的 PaymentIntent succeeded 都不替代 checkout 的 complete 与 paid 及金额身份核验。
- **固定网络路径**：一笔流程固定直连或指定 SOCKS5；代理失败不回退直连。恢复记录保存网络选择与固定端点摘要，重启后仍校验一致性，不保存代理凭据明文。
- **恢复优先原浏览器**：存在匹配活动会话时继续使用它；身份、出口或订单不符则停止。只有无歧义证据才能推进状态，未知仍为 pending/unknown。
- **报价变化撤销确认**：账单延迟出现后需要重报税价；旧报价不得继续付款。浏览器观察到的 PaymentIntent 必须与本单关联，不能接受其他订单响应。
- **需要人工验证就暂停**：3DS、银行验证和其他挑战不是可伪造的成功状态。历史方案要求用户处理验证；本文不保存绕过验证方法。
- **敏感数据最小保留**：不保存 Session、accessToken、sessionToken、Cookie、卡号、CVC、client_secret、挑战令牌、真实账单地址或代理密码。恢复用的订单引用也不在本档案保存真实值。历史 sessionStorage 只保留 checkout ID 与 processor entity，不能写入登录态或卡信息。

## 验证与恢复研究时的入口

历史测试入口包括独立仓库的 `internal/checkout/*_test.go`、`internal/checkout/browser/*_test.go`、`internal/gptpay/browser_e2e_integration_test.go` 和前端 `*_test.mjs`。核心场景需保留：双击、防重付、报价变化、异主账号、金额矛盾、断网未知状态、刷新恢复、旧记录与损坏记录、网络绑定变化和会话过期。

历史测试报告中的命令部分仍使用迁移前包路径、临时 workspace 和旧工具链。未来重跑应先按独立仓库实际布局调整，记录 commit 与 dirty diff，不照抄已失效的 `/tmp` 路径。本次没有重新运行 GPTPay 支付或 Chromium 测试。

2026-09-16 的生产准备检查显示当时未发现常见 Chromium 安装，旧服务限制为 256 MiB、128 tasks，并启用 MemoryDenyWriteExecute；当时的新浏览器执行层未上线。这是旧快照，不证明 2026-10-01 的生产状态。是否重新投入使用必须重新核验部署和真实订单结果，不能把历史合成测试当成当前验收。

## 本次清理范围

2026-10-01 从 hypitoken 移除示例配置中的 `endpoints.gptpay`、8321 说明及旧环境变量提示，删除本地 GPTPay 专用 `.hallmark` 记录和旧 `bin/gptpay` 产物。当前项目不恢复 GPTPay 源码、页面、测试入口或运行依赖；只保留本档案与 AGENTS.md 的检索入口。

hypitoken 的正常 SaaS 充值、Stripe 集成和独立商城不是 GPTPay 订阅代付模块，本次没有删除。2026-10-01 的清理不涉及独立 gptpay 仓库、Git 历史或生产服务。

## 2026-10-02 独立服务退役

按用户要求停用并删除独立 GPTPay 服务，清理其反向代理站点、专用程序、配置、数据目录、备份、证书及服务账号，同时移除旧代理配置备份中的站点块。清理前付款记录目录为空；清理后服务不存在、原监听已关闭，其他站点路由保持不变。

本研究档案、独立项目源码和 Git 历史继续保留。公开提交不包含生产主机地址、SSH 入口、机器绝对路径或生产运维记录；本次服务退役不构成对历史真实付款结果的补充验证。
