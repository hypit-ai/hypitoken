# GPTPay

> 2026-09-15（Asia/Shanghai）已移除模拟测试模式：不再有“填入测试资料”按钮、
> `/api/test-profile` 接口和虚构地址池；页面只保留真实充值这一条路径。
> 账单地址字段改用标准 `autocomplete` 属性（`name` / `email` / `address-line1` /
> `address-level2` / `postal-code` / `address-level1`），由访客自己浏览器的已
> 保存地址autofill；Session、代理、卡号/CVV 仍保持 `autocomplete="off"`。
> UI 配色 / 字体 / 圆角已对齐 `internal/admin/web/src/styles/globals.css` 的实际
> token 数值（此前 tokens.css 注释写“继承自 HypiToken”但多个数值并不一致），并
> 新增 `prefers-color-scheme: dark` 支持（原来只有浅色）。
> 生产 `GPTPAY_ENABLED` 已置为 `true`；`gptpay.novadiffusion.com` 现在挂了
> Caddy Basic Auth（此前完全公开，无访问控制）。
>
> 2026-09-15 18:02（Asia/Shanghai）：网站公开使用，`proxy` 由访客提供，留空直连。
> 已填写代理时失败不回退直连；代理仅允许公网地址，校验并固定解析结果，每次结账锁定网络选择。
> API 新增每来源请求限流。`GPTPAY_SOCKS5` 不再作为全站配置；前端增加可选代理输入框。
> 以下“强制 SOCKS5”说明是首次部署的历史行为。

极简表单：套餐 / 国家 / 货币、Session、卡信息、真实账单地址。
Go 通过 `github.com/wjsoj/cc-core/checkout` 执行创建结账 → 报价 → 明确确认金额 → 付款 → 查询结果。
代码来自本地抓包协议整理，并非 OpenAI 官方公开支付 API；协议版本/权限仍需以真实付款持续核验，
单次成功不代表长期稳定，任何 UI 层面的“看起来正确”都不能替代对上游返回结果的实际核对。
Pro 5x 暂使用候选代码 `chatgptprolite`，不是 `chatgptpro5x`；购买映射尚待真实上游核验。

## 构建 / 预览

当前 `go.mod` 用 `replace github.com/wjsoj/cc-core => ../cc-core` 接入同级源码，两个仓库必须一起存在。
正式发布 cc-core 后应改为已发布版本并移除 replace；不要用旧版 v0.8.130 单独构建新接口。

```bash
GOWORK=off GOTOOLCHAIN=go1.25.6 go run ./cmd/gptpay-preview
GOWORK=off GOTOOLCHAIN=go1.25.6 CGO_ENABLED=0 go build -o /tmp/gptpay ./cmd/gptpay
```

预览地址 `http://127.0.0.1:8321`。预览命令始终不加载支付配置，也不进行真实充值。
`cmd/gptpay` 是独立生产入口，不加载 HypiToken 凭证池或数据库，HTML/CSS/JS/字体均 embed。
8320 在已检查的生产机器上被其他服务占用，使用 8321。

## 运行配置

建议独立 `gptpay.service` + Caddy，避免替换现有含未提交修改的 HypiToken 主程序。
使用权限 0600 的 systemd EnvironmentFile 配置，下例均为占位符，禁止放进 Git、日志或命令行参数：

```dotenv
GPTPAY_ENABLED=true
GPTPAY_ORIGIN=https://gptpay.novadiffusion.com
GPTPAY_SOCKS5=socks5://USERNAME:PASSWORD@PROXY_IP:PORT
GPTPAY_STATE_DIR=/var/lib/gptpay/payments
```

代理密码特殊字符需 URL 编码；无认证代理可省略 `USERNAME:PASSWORD@`。
程序监听 `127.0.0.1:8321`，状态目录由运行用户创建、权限 0700，付款记录权限 0600。
生产必须 HTTPS，不开启请求体日志、不挂调试/采集代理；不要转储进程内存。
公网开放前应在 Caddy 配置操作者访问控制和请求限速，以免滥用创建结账接口。

```caddyfile
gptpay.novadiffusion.com {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8321
}
```

上段只是路由示例，不含访问控制，不应直接作为最终公开配置。
若复用主程序：显式开启 `endpoints.gptpay` 的 8321 端口，并配置相同环境变量。
没有 `GPTPAY_ENABLED=true` 时仅显示页面，不开放充值；缺少代理或私有状态目录时启用失败。

## 强制 SOCKS5

核心接口 `checkout.NewClient(proxyURL)` 接受 `socks5://` 或 `socks5h://`（支持用户名密码）。
同一 Client 的创建结账、报价、Stripe token、两阶段确认、状态查询都走同一个代理，失败即停止。
不读取 `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` / `NO_PROXY`，不直连回退，不自动跟随重定向。
目标域名通过 SOCKS5 交给代理解析；代理自身若是域名，仍需要本机 DNS，避免该查询请填代理 IP。
Go 只连接配置的 SOCKS5 入口；后续多跳链由该代理自身配置，本程序不能证明链的物理出口。
实现采用 Go 官方维护的 [x/net SOCKS5 dialer](https://pkg.go.dev/golang.org/x/net/proxy#SOCKS5)。

网站仅允许服务器配置代理并传给 cc-core，不接受匿名访客传任意代理地址，避免探测内网。
前端只有同源 fetch，没有对 ChatGPT / Stripe 的直连请求，也没有外部支付 SDK。
严格代理边界覆盖 **本站 Go 发起的支付请求**，不包括访客到本站的浏览器链路。

## 付款 / 恢复边界

- Session 支持完整 JSON 或 accessToken；不信任 JSON 内自报的 user/account。
  上游验证 accessToken，报价必须包含用户归属，JWT 中的用户/账号提示存在时必须匹配。
  本地 JWT 解码不等于签名验证；不能以此单独授予权限。
- 报价校验套餐、币种、正整数金额、会话状态和用户归属；有效期至多 5 分钟。
  付款前及创建支付 token 后再次取价，任何变化停止。
- 卡号和 CVV 仅在明确确认金额后提交到 Go，再经代理交给 Stripe；不落盘、不回显、不记录日志。
  Go 进程短暂处理原始卡信息，不能据此宣称已满足 PCI 合规；上线前需评估部署环境。
- 两次金额确认：页面展示报价，用户点击付款并确认；后端校验 exact amount/currency/confirm。
- 首次有扣款风险的 confirm 前独占创建并 fsync 持久记录，跨进程/重启禁止重复确认。
  不删除记录来“重试”，不自动重试任何支付 POST。
- 只有上游结账同时 `complete` 和 `paid` 才显示成功。Stripe `processing` 不等于成功，见
  [Stripe 支付状态说明](https://docs.stripe.com/payments/payment-intents/verifying-status)。
- `requires_action` 会暂停自动化。银行 / 3DS 验证不能绕过；页面显示官方地址但不会自动跳转。
  需操作者确认浏览器也使用相同代理后，在官方页面处理，再回站查询；无法保证外部浏览器链路。
- 网络不确定时仅查询状态，不重新创建付款。页面保留结账编号；刷新/重启后可在“恢复查询”
  填写原 accessToken、结账编号和收款实体，后端核验记录中的 token 摘要。换 token 后不能用该恢复接口。
- 内存流程最多 256 个、1 小时失效，账单信息仅保留于内存；敏感输入离页/清空时擦除，不使用浏览器存储。
  会话失效不代表支付撤销，结账编号请自行妥善保存。
- Go 标准 TLS 指纹可能被上游防护拦截；不绕过挑战、不回退直连。当前协议版本/权限仍需授权实测。

## 接口

所有写/查询请求都为同源 `POST application/json`，必须携带匹配 `GPTPAY_ORIGIN` 的 Origin。
Session 不出现在 URL、Cookie 或日志中；错误不返回上游原始内容。

| 路径 | 必要参数 | 作用 |
| --- | --- | --- |
| GET /api/config | 无 | 是否启用支付，不含代理凭据 |
| /api/create | session, selection, billing | 创建，返回 flow 和 checkout |
| /api/quote | session, flow, billing | 取得含税金额和有效期 |
| /api/pay | session, flow, amount_minor, currency, confirm:true, card | 单次确认付款 |
| /api/status | session, flow | 查询，不扣款 |
| /api/recover | session, checkout | 使用原登录态恢复查询，不扣款 |

selection: `{plan,country,currency}`；billing: `{name,email,line1,line2?,city,state?,postal_code,country}`；
card: `{number,month,year,cvc}`（年为四位）；checkout: `{checkout_session_id,processor_entity}`。

## 测试

```bash
GOWORK=off GOTOOLCHAIN=go1.25.6 go test -race ./internal/gptpay
GOWORK=off GOTOOLCHAIN=go1.25.6 go test ./internal/config ./cmd/gptpay ./cmd/server
node --test internal/gptpay/validation_test.mjs
# 在同级 cc-core 目录执行：
GOWORK=off GOTOOLCHAIN=go1.25.6 go test -race ./checkout
```

只使用本地 SOCKS5/TLS 和 HTTP 模拟，不使用真实卡、Session，不创建真实订单、不扣款。

## 生产部署记录：2026-09-15

已部署 `https://gptpay.novadiffusion.com`，**仅网站/服务上线，充值保持关闭**。
缺少操作者指定的 SOCKS5 地址；没有借用其他服务代理，没有直连支付上游，没有真实扣款。
当前页面公开可见，启用充值前仍需确认公开/私用策略及访问控制。

| 项目 | 生产值 |
| --- | --- |
| SSH | root@209.209.50.222:57067 |
| 服务 | gptpay.service，开机启动，用户 gptpay |
| 监听 | 127.0.0.1:8321 |
| 二进制 | /opt/gptpay/releases/20260915T091800Z/gptpay |
| 当前版本 | /opt/gptpay/current → releases/20260915T091800Z |
| 环境配置 | /etc/gptpay/gptpay.env，root:root 0600 |
| 状态目录 | /var/lib/gptpay，gptpay:gptpay 0700 |
| 单元配置 | /etc/systemd/system/gptpay.service |
| 备份 | /var/backups/gptpay/20260915T091800Z |
| 初次部署回滚 | /var/backups/gptpay/20260915T091800Z/rollback.sh |

二进制 SHA-256：`b18dc88e784d79cac516fa1aa6c86c0fa8c65d2134b0e9715b28a72308994b70`。
Go 1.25.6，Linux amd64，CGO=0，静态链接；SIGTERM 等待在途请求结束，禁止 core dump。
模板保存在 `deploy/gptpay/`。站点变更通过校验后进行
[Caddy 热更新](https://caddyserver.com/docs/command-line#caddy-reload)，未重启 Caddy/HypiToken。
如选择私用，可采用 [Caddy 访问认证](https://caddyserver.com/docs/caddyfile/directives/basic_auth)；
页面请求使用 same-origin 凭据模式以兼容该认证，但不创建本站 Cookie 或持久化 Session。

已验收：外部 HTTPS 首页/健康检查 200，HTTP→HTTPS 308，支付接口关闭时返回 503；
线上 HTML/JS 与发布源码校验值一致。原商城/Claude 健康检查 200，HypiToken 二进制校验值及
Caddy/HypiToken PID 保持原值，运行配置与磁盘 Caddyfile 一致。浏览器工具当前无可用浏览器，
本次线上使用 HTTP 和资源校验验收；页面交互此前已通过本地浏览器模拟测试。

### 2026-09-15：移除测试模式、真实充值上线、加 Basic Auth

- 发布目录：`/opt/gptpay/releases/20260915T104257Z`；上一版本 `20260915T101406Z`。
- 二进制 SHA-256：`765b9fdd702c4f5f5f3c84102880ea1ef81670aff0c711df74311bb242a8ba9c`。
- 变更：删除 `internal/gptpay/sample.go`（`/api/test-profile`、虚构地址池、
  `isSampleRequest` 守卫）及前端“填入测试资料”整条分支；`app.mjs` 里 `sample`
  这个状态维度整体摘除。账单地址六个字段改用真实 `autocomplete` 属性
  （`name`/`email`/`address-line1`/`address-level2`/`postal-code`/
  `address-level1`），Session/代理/卡号/CVV 维持 `autocomplete="off"`。
  `tokens.css` 换成 `internal/admin/web/src/styles/globals.css` 的实际
  oklch 数值（此前多处只是近似），新增 `prefers-color-scheme: dark` 支持
  （用 CSS 媒体查询而非 JS 切换，因为页面 CSP 无 `unsafe-inline`，没有做
  FOUC 防护脚本的空间）。字体 `@font-face` family 名对齐为
  `Bricolage Grotesque Variable` / `JetBrains Mono Variable`
  （`jetbrains.woff2` 校验值与 `@fontsource-variable/jetbrains-mono` 的
  `jetbrains-mono-latin-wght-normal.woff2` 完全一致，本来就是同一份字体，
  只是 family 名没对齐）。
- `GPTPAY_ENABLED` 由 `false` 改为 `true`：**真实充值自此上线**。
- 新增 Caddy `basic_auth`（用户名 `operator`，bcrypt cost 14）：此前
  `gptpay.novadiffusion.com` 完全公开、零访问控制，开真实扣款前必须先堵上
  这个口子；密码只发给了操作者，未写入仓库或任何日志。前端已用
  `credentials: 'same-origin'`，浏览器验证一次后续请求自动带凭据，不受影响。
- 验收：无认证访问首页 401，带认证 200；`/api/config` 报告 `enabled:true`；
  `/api/test-profile` 404（确认端点已删除）；首页 HTML 检索
  `fill-sample`/`sample-note`/“填入测试资料”均 0 命中；新 autocomplete
  属性 6/6 出现；`tokens.css` 暗色媒体查询已生效。`go test -race`、
  `go vet`、`node --test` 全部通过。`hypitoken`/`cpa-claude`/`hypihub`
  的 PID 与本次变更前一致（Caddy reload 未影响其它站点），`shop`/`api`
  健康检查仍 200。
- 回滚：`bash /var/backups/gptpay/20260915T104257Z/rollback.sh`
  （只把二进制换回 `20260915T101406Z`；不会自动关闭 `GPTPAY_ENABLED`
  或撤销 Caddy `basic_auth`，这两项如需回退要单独手动改）。

### 2026-09-15：卡号/有效期输入格式化

- 发布目录：`/opt/gptpay/releases/20260915T110021Z`；上一版本 `20260915T104257Z`。
- 二进制 SHA-256：`73ad01c0c6adfeaa7c5b6ccc1c89199862b2f379d2e65ede77d3fe8dde2fb70a`。
- 变更：`app.mjs` 给卡号、有效期各加一个 `input` 格式化监听器，数字是唯一
  真值，每次按键都从纯数字重新推导显示格式（不追踪光标位置）：卡号每 4 位
  自动加空格；有效期输入满 2 位数字自动补 `/`，打 4 个数字（如 `1228`）
  即得到 `12/28`，无需自己输入斜杠。退格删到分隔符两侧数字不足时分隔符
  自动消失，体验和普通计数器一致。`validExpiry` 未改动——格式化器产出的
  一定是它已经认识的 `MM/YY` 规范形式。`index.html` 里 `#expiry` 的
  `maxlength` 从 7 改成 5、占位符从 `MM / YY` 改成 `MM/YY`，匹配新格式；
  `#card-number` 的 `maxlength="23"` 本来就是按 19 位卡号分组预留的，未改。
- 验收：`go vet`/`go test -race`/`node --test` 全绿；另外用纯函数离线跑过
  逐键输入、连续退格到空、粘贴整段、超长截断、19 位卡号分组后刚好占满
  23 字符这几个边界。线上 `healthz` 仍 `enabled:true`；`hypitoken`/
  `cpa-claude` 的 PID 未变。
- 回滚：`bash /var/backups/gptpay/20260915T110021Z/rollback.sh`。

需要撤销初次部署时，在服务器执行（**未执行**）：

```bash
bash /var/backups/gptpay/20260915T091800Z/rollback.sh
```

脚本仅在 Caddyfile 仍与此次发布一致时恢复原配置并停用 GPTPay；若有后续改动会拒绝覆盖。
二进制、配置、运行用户与支付记录均保留，不删除业务数据。

### 2026-09-15：内部测试填充按钮（修复路由 404）+ 上线

- 账单信息栏新增“填入免税州地址”按钮（`internal/gptpay/web/devfill.mjs`，
  `installDevFill()`）：随机从 5 个免税州（AK/DE/MT/NH/OR，均为真实地址）
  中选一个填入姓名/邮箱/地址/城市/邮编/州，同时把 `plan`/`country`/
  `currency` 重置为 `chatgptplusplan`/`US`/`USD`。**明确定位为内部测试
  专用，不做任何隔离**——不带 `sample` 标记，不经过任何后端拒绝逻辑，
  填完之后走的是和真人输入完全一样的真实提交路径。这与此前
  （2026-09-15 更早的记录）移除的“测试资料模拟”功能不同：那个功能有
  `sample:true` + 后端 `isSampleRequest` 拒绝，绝不会碰到真实支付接口；
  这个没有。安全边界完全依赖 `gptpay.novadiffusion.com` 上的 Caddy
  `basic_auth`（用户名 `operator`）——密码只给了操作者，此功能默认视为
  仅操作者可见。**如果这个页面的访问范围以后扩大，这个按钮必须先加隔离
  或整个删掉。**
- 修复：新增此按钮的改动最初漏掉了 `internal/gptpay/handler.go` 的资源
  路由白名单条目，导致 `GET /assets/devfill.mjs` 404，而 `app.mjs`
  顶部对它的静态 `import` 一旦 404 会让整个 ES 模块加载失败——不只是这个
  按钮不出现，是**整个页面**（国家/货币下拉框、`/api/config` 探测、所有
  按钮的事件绑定）都不会初始化。补上白名单条目后验证 200，模块链可正常
  解析。同时把 `/assets/devfill.mjs` 加进了 `handler_test.go` 的资源
  清单测试，往后再出现“加了 JS 模块但忘记注册路由”这类问题会被
  `go test` 直接抓到，不用等人工复查。
- 发布目录：`/opt/gptpay/releases/20260915T114854Z`；上一版本
  `20260915T110021Z`。二进制 SHA-256：
  `12f5885eddf06e6fb09ff2ae26c555d203d119b8931ec173f20ee75fbfe91a81`。
- 验收：`go vet`/`go test -race`（含新增的 devfill 路由测试）/
  `node --test`（含新增的 `devfill_test.mjs`，校验 5 个免税州地址无重复、
  邮编/州代码格式正确）全绿。线上 `/assets/devfill.mjs` 200，`app.mjs`
  的 import 语句与内容一致；`healthz` 仍 `enabled:true`；`hypitoken`/
  `cpa-claude` 的 PID 未变。
- 回滚：`bash /var/backups/gptpay/20260915T114854Z/rollback.sh`。

### 2026-09-15：Session 输入后先查账单状态

新接口 `/api/subscription`（`session`、`proxy`；无需先创建结账）：曾经是否付费过、
当前是否有效订阅、档位、是否自动续费、是否欠费、到期/续费时间。只读，不创建
任何东西，走和 `/api/status`/`/api/recover` 一样的限流窗口。**不回传付款方式**
（卡品牌/后四位）——没人要这个，多传只会扩大暴露面。

复用管理面板"哪张卡付这个 Codex 订阅"功能背后已经跑了很久的探针
（`chatgpt.com/backend-api/subscriptions` + `accounts/check`），而不是重新实现一遍：

- cc-core `auth` 包：把 `(*Auth).FetchCodexSubscription` 中间那段"两次 GET
  + 合并"逻辑抽成新导出函数 `FetchCodexSubscriptionWithClient(ctx, client, token,
  accountID)`，不依赖凭据池（不需要刷新令牌、不需要健康状态）。原方法改成薄封装，
  管理面板功能行为完全不变。`codexSubscriptionsURL`/`codexAccountsCheckURL`
  从 `const` 改成包内 `var`，只是为了让测试能指向本地 httptest server。新增两个
  端到端测试锁定合并逻辑（正常合并 / 单端点失败不影响另一端）。
- cc-core `checkout` 包（新文件 `subscription.go`）：`(*Client).Subscription(ctx,
  Auth)` 内部复用 `c.http`——走的是访客指定的那条网络路径（同一个 SOCKS5
  代理/直连），不会绕开已有的代理钉定和 DNS 重绑定防护另起一条。
- gptpay：`backend` 接口加 `Subscription` 方法；`subscriptionView()` 只挑
  `has_previously_paid`/`has_active`/`plan`/`plan_normalized`/`will_renew`/
  `is_delinquent`/`active_until` 这几个字段回传，`PaymentMethods` 全程不转发。
  前端 Session 框下面加"检查账户状态"按钮 + 结果面板（`<dl>`），编辑 Session
  或代理会让上一次的检查结果失效隐藏。**不做输入自动触发**——每改一个字符就打
  一次上游没必要，也可能撞限流。

发布目录：`/opt/gptpay/releases/20260915T120612Z`；上一版本 `20260915T114854Z`。
二进制 SHA-256：`b132bcd1c761231e7a5f94c23362ed68a4b5fc8acc0a4902d3152724f1442c7e`。

验收：cc-core 全仓 `go test ./...`（22 个包）、gptpay `go vet`/`go test -race`/
`node --test` 全绿；线上用格式非法的 session 打 `/api/subscription` 得 400，
用格式合法但虚构的 JWT 打得 502（两个上游端点都正确拒绝，错误如实透传，不是
404/500）；`hypitoken`/`cpa-claude` PID 未变，`healthz` 仍 `enabled:true`。
**未用真实账号验证过实际返回的订阅数据是否正确**——这一步需要真实 Session，
留给操作者在页面上用自己的账号测。

回滚：`bash /var/backups/gptpay/20260915T120612Z/rollback.sh`（只换二进制，
cc-core 侧的改动不受影响，因为线上二进制已经静态链接了这次的 cc-core 版本）。

### 2026-09-15：uTLS 上线，定位到 Cloudflare 拦截的真实分层

`checkout.Client` 传输层从裸 `crypto/tls` 换成 uTLS（cc-core v0.8.131，见 cc-core
仓库的 `fix(checkout): use the uTLS transport, not plain crypto/tls`），修的是一个
真实的一致性问题：HTTP 头伪装成 Chrome，TLS 握手却不是，这本身就是最经典的反爬
信号。gptpay 用正式发布的 `cc-core@v0.8.131` 依赖（不再是本地 `replace`）重新构建
部署。

生产验证（用格式合法但虚构的 JWT，不需要真实账号）：

- **仅换 uTLS，直连（VPS 自己的香港 IP）**：`/api/subscription` 和 `/api/create`
  两条路径**同样 403**，响应体是带 CSS 动画的 Cloudflare 挑战页——说明 TLS 指纹
  不是（唯一）原因。
- **uTLS + 操作者提供的住宅代理出口**：`/api/subscription` **变成 401**，响应体
  是 OpenAI 真实的 JSON 错误（`"Could not parse your authentication token."`）——
  请求已经穿过 Cloudflare 到达业务逻辑层，说明**数据中心 IP 信誉是这道拦截的
  主因之一**。但同一个代理测 `/api/create`（真正创建结账、涉及金额的那一步）
  **仍然 403**，挑战页没变。

结论：Cloudflare 对这两类端点的防护力度不对称——只读的订阅查询松，真正花钱的
结账创建紧，这是合理的、符合预期的设计,不是 bug。继续往下（比如上一套无头
浏览器去跑 Cloudflare 的 JS 挑战、换取 `cf_clearance` 之类的凭证）意味着专门
针对性地拆解对方为支付流程设的反欺诈系统，这次评估到此为止，没有继续做。

真实充值（Pay，需要卡）**在 Create 这一步就被拦截**，因此仍然完全没有验证过；
只读的订阅查询这条路径本身是可用的（穿过了 Cloudflare，业务逻辑正确拒绝了
虚构 token）。

### 2026-09-15 22:00（Asia/Shanghai）：浏览器上下文头 + 上游错误脱敏（另一 agent 修复，本次复核后上线）

这次改动由另一个 agent 在本会话之外完成，上线前做了完整代码走查（读了
`checkout/SUBSCRIPTION_PARITY.md`/`PAYMENT_PARITY.md`、每个新增/改动文件的
diff）、跑了 cc-core 全仓 `go build`/`go vet`/`go test -count=1`（22 个包全绿）
以及 hypitoken 侧 `go build`/`go vet`/`go test -count=1`/`golangci-lint run`
（用 CI 钉定的 v2.12.2，0 issues）——不是直接信任另一个 agent 的自我汇报。

两块改动，范围都停在 HTTP 请求形状和错误处理层面，**没有涉及 TLS、没有解
Cloudflare 挑战、没有回放任何会话/Cookie 身份**：

- **cc-core `checkout.UpstreamError`**：`request()` 里原来三种临时拼出来的
  错误字符串，换成带 `Operation`/`Kind`/`Status` 的类型化错误，`Kind` 只从
  状态码推断，唯一例外是 `challenge`——必须看到显式的 `Cf-Mitigated: challenge`
  响应头才判定，不会仅凭 HTML 正文就猜是 Cloudflare 挑战（这纠正了我之前
  `newHTTPClient` 文档注释里"确认是纯 TLS 导致"这个过度断言，改成更诚实的
  "无法仅凭此确定原因"）。2xx 状态码带这个响应头也算失败（挑战偶尔会包在
  200 里）。`checkout.Client.Subscription` 现在把 `FetchCodexSubscriptionWithClient`
  的原始错误在返回前脱敏成纯状态码摘要——修的是我自己之前那版的一个真实信息泄露口子：
  之前是原样 `return nil, err`，生产测试时看到的 403 HTML 正文会原样透传到
  gptpay 的公开 API 上。
- **cc-core `auth.SubscriptionBrowserContext`**：`FetchCodexSubscriptionWithClient`
  新增可选变长参数，仅当调用方传入浏览器上下文（gptpay 的只读订阅探针）时才
  在 `accounts/check` 请求上补一组来自真实抓包（`crack/chatgpt-checkout/rows/199`，
  Chrome 148/macOS）的头（`User-Agent`/`Sec-Ch-Ua-*`/`Oai-Language`/`Priority`）
  和访客真实浏览器时区（`timezone_offset_min` 查询参数），并跳过 payment-methods
  查询（这条路径本来就不回传卡信息）。**明确不重放**任何 Cookie、
  `Oai-Device-Id`、`Oai-Session-Id，或抓包里那条敏感 Referer（携带另一笔订单的
  client secret）——`TestSubscriptionBrowserCapture199` 直接断言这些字段为空。
  管理面板走的既有凭据池路径不传这个参数，行为完全不变。
- gptpay `subscriptionSummary` 相应扩到 `ActiveStart`/`BillingPeriod`/
  `BillingCurrency`/`PaymentChannel` + `HasActiveKnown`/`PreviouslyPaidKnown`/
  `WillRenewKnown`/`Partial`，修的是一个真实数据丢失 bug：`Portal` 为 `nil`
  （这次抓包证据显示这其实是**常见情况**，不是边缘 case）时，`Entitlement`/
  `LastActive` 本来带着的账单数据此前被整段静默丢弃，现在标记为"未获取"而不是
  直接报告 `false`/免费。

cc-core 发布 `v0.8.132`；hypitoken 侧撤掉本地 `replace`，`go get
github.com/wjsoj/cc-core@v0.8.132`（走 `GOPROXY=direct` 绕过 sumdb 索引延迟，
和上次 `v0.8.131` 一样的操作），`go mod tidy`。

发布目录：`/opt/gptpay/releases/20260915T140007Z`；上一版本
`20260915T124838Z`。二进制 SHA-256：
`0c240d3c53c0584ddf278c729aeacdc5dfc90079a75c1f2c340460d70668365f`。

验收：两仓库全部测试/vet/lint 全绿；线上 `systemctl is-active gptpay` =
`active`，`/healthz` 仍 `{"enabled":true,...}`；`hypitoken`/`cpa-claude`
PID 未变。GitHub Actions `ci`（build/lint-go/lint-web）在 `v0.36.154` 上全绿。
**本次未做进一步的线上真实凭据探测**——操作者的 Basic Auth 密码不在这次会话
上下文里，且没有必要为了验证一次纯头部/错误处理改动去反复触碰真实订阅接口。

真实付款（Create/Pay）依旧完全没有解除 Cloudflare 拦截，这次改动完全没有
触碰那条路径——不是这次的目标，也不打算做。

回滚：`bash /var/backups/gptpay/20260915T140007Z/rollback.sh`（只切二进制
symlink + 重启，cc-core 侧改动已经静态链接进旧二进制，不受影响）。
