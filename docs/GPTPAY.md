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
