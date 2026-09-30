# Salcara Hub：独立设备扫码配对插件

插件版本 `0.3.1`，内核 Hub `1.4.0`，公开远程协议 `salcara-remote` v1。
站点运营者各自部署；不依赖 Salcara.top 的中心账号服务，也不要求使用者有本站账号、本站模型 API Key、余额或指定分组。

## 开源部署边界

本目录是可独立打包的 Sub2API 插件源码，使用宿主的插件 HTTP 转发能力 `salcara.hub.http.v1`。
当前 manifest 面向 **已经合入本项目 Hub 转发扩展的自定义 Sub2API 0.2.11+salcara.hub.1 宿主**，并非任意原版 Sub2API 装入 ZIP 就能工作。版本范围只是基础版本检查，不证明原版宿主具备该能力；原版 0.2.11 仍只支持 OAuth outbound capability。不要为了安装此插件将已有 0.2.11 服务器降级到早期离线研究所用的 0.2.8。
未提供该扩展的宿主需要先合入相关路由、插件能力与 UI 桥接改动；客户端探测失败时应明确显示“插件未安装 / 不兼容”，不能假装已连接。

设备模式不再调用 `/v1/salcara-hub/key`，因此无需给原版 Sub2API 添加任何“用户资格校验”接口。
旧版 API-Key 客户端的分组验证代码保留为可选兼容路径，默认空分组不授予旧版 Key 模式权限；这 **不会禁用新设备扫码模式**。
插件管理 UI 的“检查配置”只验证本地配置和宿主数据目录要求，不证明公网地址、HTTPS、反代或旧版验证接口已经可达。

## 连接流程与接口

每个站点公开相同接口前缀 `/salcara-hub/v1`。客户端首先用用户输入的站点 origin 探测：

1. `GET /ping`：要求 `service=salcara-hub`、`protocol=salcara-remote`、`protocolVersion=1`，并识别 `device.identity.v1`、`pair.qr.v1` 等能力。
2. 电脑生成随机设备 ID 和独立 32 字节设备密钥；以 `X-Salcara-Device-Id`、`X-Salcara-Device-Secret` 调用 `POST /device/register`。不发送模型 Key / 账号 Bearer。
3. 电脑用相同设备凭证建立 `GET /bridge/stream?deviceId=...` SSE；服务端每台电脑只保留最新一个流。
4. 电脑 `POST /bridge/pair/start`，JSON `{"deviceId":"..."}`，获得 5 分钟有效的 `ticket`、`expires_at` 和旧版人工输入 `code`。二维码由客户端包装站点、设备 ID 与 ticket。
5. 手机 `POST /app/pair/qr`，JSON `{"deviceId":"...","ticket":"..."}`，无需先登录站点。ticket 只能成功领取一次，电脑离线时拒绝配对。
6. 返回 `pair_token` 后，手机仅以 `X-Salcara-Pair-Token` 访问 `/app/devices`、`/app/stream`、`/app/events`、`/app/commands`。此 token 不能登记电脑、建立 bridge 流或调用宿主后台。
7. 电脑 `POST /bridge/pair/revoke` 可撤销绑定；手机 `POST /app/pair/revoke` 可解除自身绑定（无需请求体，也不能指定其他电脑）。撤销同时使 token、待用二维码和旧手机流失效。

每台电脑独立命名空间，手机只看到并操作它绑定的电脑。重新扫码会轮换手机 token，旧手机立即失去后续请求权限。
客户端按站点保存连接凭证；换另一个中转站需重新探测与配对，不可把原站点 token 发给新站点。
电脑本地模型密钥库与远程设备身份是两套东西；更换模型 Key 不需要重新扫码。远程更换 Key / 重启工具属于电脑端命令能力，不能仅因 Hub 在线就声称工具已支持。

## 按需访问与断线恢复（2026-09-30）

手机默认不保留 `/app/stream` 常驻 SSE；进入会话、读取更新或发送任务时才使用短 HTTP 请求。电脑因 NAT 和唤醒需要仍保留每设备一条轻量 `/bridge/stream`，心跳不携带会话正文。手机离开页面或退到后台应停止读请求；旧 SSE 接口保留兼容，不能把「无手机 SSE」解释为服务器零连接、零请求或无带宽成本。

`GET /ping` 新增 `events.cursor.v1`；只有数据目录可写且安全记录已成功加载时才公布 `commands.idempotency.v1` 与 `commandIdempotencyTTLSeconds:86400`。未公布时手机不得自动重放可能已送达的修改类指令。测试夹具默认不设数据目录时不会公布安全重试能力。

修改类 `POST /app/commands` 可增加顶层 `requestId:<标准小写UUID>`。一个发送动作产生一个 ID，网络恢复后重试必须保留相同 ID、设备、站点、配对凭据和完全相同的 command；不能生成新 ID 来「恢复」。轮询状态、列表和读会话不需要 requestId，以免浪费安全记录。旧客户端省略 ID 的行为不变，但不具备这一层安全重试。

服务端以站点实例、账号命名空间、配对身份、设备、requestId 和规范化 command 区分请求；指令投递前同步写入并 fsync 哈希安全记录。手机 HTTP 断开不取消已投递任务，后续同 ID 请求等待原执行结果或读取缓存；同 ID 换正文返回 409 `request_id_conflict`。结果释放前重新校验配对及封禁状态。

安全记录只保存身份 / 正文的不可逆哈希和到期时间，**不**写入模型 Key、设备 / 手机秘密、prompt 或回复。每站最多 4,096 条、每配对身份最多 256 条，保留 24 小时；同一运行进程回复缓存总计最多 16 MiB。超限拒绝新投递，不驱逐未到期记录。数据目录必须使用支持原子硬链接、fsync 的本地文件系统，每个目录只能由一个 Hub 实例管理；磁盘不可写时拒绝指令而不假装成功。

Hub 重启后的既存记录、电脑断线后投递是否成功不明、等待超时或缓存已回收时返回 409：
`{error,code:"command_delivery_uncertain",retryable:false,requestId}`。
同 ID 永不再次投递；手机应刷新电脑的**原会话**核实，而不是把它当作肯定没发送。执行前已确认电脑离线则返回 `computer_offline,retryable:true`，没有投递也不建立记录，恢复后可用原 ID 再试。24 小时之外、换站点 / 重新配对 / 管理员删除数据目录不在去重保证内，手机应停止自动重放。这里是有时间边界的防重复，不是跨所有灾难场景的永久 exactly-once。

`GET /app/events?deviceId=...&sessionKey=...&after=...&limit=100` 直接读取缓存游标，不需要打开手机 SSE；兼容默认最多 500 条，limit 可选 1–500，单页正文约束 2 MiB。返回：
`{events,nextSeq,lastSeq,oldestSeq,hasMore,resetRequired}`。
手机处理完整 JSON 页后再推进 nextSeq；请求收一半断开不推进，恢复后从原游标读取。同一事件按 seq 去重，message 本身依照 ID 全量替换，不拼接重复片段。hasMore 表示可继续分页；resetRequired 表示缓存截断 / 重启 / 游标来自未来，应通过 session.open（桌面为 desktop.session.open）重新加载原会话。需要设备范围事件时必须显式加 `scope=device` 并省略 sessionKey；插件始终验证该设备与手机绑定，不返回其他设备事件。

任务的 JSON 正文须完整校验后才投递，不向电脑发送半条指令。图片 / 文件的大文件断点上传不在此文字指令接口范围内。电脑侧任务不依赖手机保持在线，回复恢复依赖当前缓存和电脑原记录；服务器事件缓存并非永久聊天备份。

## 安全与隐私限制

- **当前不是端到端加密。** HTTPS 保护传输，但站点服务器可以读取转发的会话事件、工具操作及敏感控制请求。用户必须信任所连接站点；不要宣传“站点无法看到内容”。
- 设备秘密和手机 token 的凭证字段只持久化 SHA-256 哈希；二维码 / 人工配对码只保存在内存中且过期失效。请求日志不包含凭证头、二维码 ticket、请求正文或 URL 查询参数。
- 每 IP 每分钟最多首次登记 10 台电脑；错误身份验证默认每分钟 20 次。账号命名空间最多 10,000 个，设备登记正文最多 64 KiB，待处理指令全站最多 4,096。它们是应用级保护，不替代公网防滥用和站点容量规划。
- Sub2API 的插件传输协议不会传递 `RemoteAddr`。配套宿主路由会覆盖 `X-Salcara-Peer-IP` 为 **实际 socket peer**，移除客户端 `X-Forwarded-For` / `X-Real-IP` / `Forwarded`。插件只消费该宿主字段，验证 IP 后填入本地 `RemoteAddr`；默认 `trust_proxy=false`，不会采用公网请求自行声明的来源 IP。
- 若宿主前面是 nginx，默认按 nginx socket IP 分桶，多个真实用户可能共用限流窗口。缺少新版宿主来源字段时按 `unknown-host-peer` 共用一桶。不能声称这种配置有可靠的逐用户反暴力保护。
- 要获得更精准的真实客户端分桶，需要在宿主明确配置可信代理范围、校验转发链、仅由宿主覆盖来源字段，并增加相关测试；本版本刻意不直接采用 `c.ClientIP()` 或用户 XFF。应在公网代理层额外设置 `/device/register`、`/app/pair/qr` 及总体请求速率、连接数限制。
- 手机撤销或换绑不能“撤回”已经送达电脑的命令；桌面端应对切换凭证、重启、文件修改等敏感命令提供明确权限与运行中保护。
- 持久化数据目录应由站点专属 OS 用户管理、限制权限并备份。插件管理 UI 只展示聚合运行数字，不展示用户会话正文或凭证；站点运营者的服务器权限仍然允许读取服务进程与存储。

## 本地验证（不调用付费模型）

在此目录运行 `go test ./...` 和 `go vet ./...`。测试覆盖无站点 Key 验证的登记、跨设备隔离、手机 token 不能作为 bridge 身份、并发登记与单次扫码、过期 / 重放 / 轮换 / 撤销、哈希持久化与重启、IP 限流、离线不假成功，以及日志不输出凭证。

`go run ./tools/device-smoke -listen 127.0.0.1:47837` 启动仅监听 loopback 的真实 Hub 测试夹具，默认不持久化，不访问上游模型接口。
需要验证重启保留绑定时加 `-data-dir` 并提供明确的临时测试目录；不要使用生产插件数据目录。
可用 `GET http://127.0.0.1:47837/salcara-hub/v1/ping` 检查发现字段；stdout 显示监听地址，Hub 日志可观察登记、bridge 连接 / 断开、请求状态。
该工具是本地互操作测试辅助程序，不是生产独立部署入口。

宿主升级、代理 TLS 配置和公网部署须由站点运营者独立完成；本次已生成独立本地发布者签名的测试包，但没有发布或部署到任何线上站点。

## 管理面板与统计口径

插件配置页现在提供分页设备表、名字 / 系统搜索、在线 / 离线 / 已配对 / 已封禁筛选、单机往返测量、封禁 / 解封 / 撤销绑定 / 临时断开，以及最近管理审计。
接口默认每页 100、最多 200 台，UI 每页 50；审计持久化保留最近 200 条、页面展示最近 50 条。

设备配对模式无法准确统计去重的自然人用户数：没有本站用户登录，也没有同一人在多手机 / 多电脑之间的身份关联。
`accounts` 是内部隔离命名空间数，**不是注册人数或活跃人数**。
面板展示的是已登记电脑数、已配对设备数、在线电脑 SSE 数、活跃手机 SSE 连接数、在线电脑最近上报的运行中 / 待审批会话、待处理指令和错误请求。
一个手机打开多个连接会计为多个 SSE；离线或已封禁电脑不计为在线 / 运行中，已封禁设备不计为有效绑定。
会话计数仅依赖 `session.updated` 状态，不枚举进程，也不在管理页面展示标题、正文、提示词、项目路径或原始 Key / token。

命令成功、失败回复、超时、公开 Hub HTTP 4xx / 5xx 及 RTT 样本是本次 Hub 进程的运行指标，不是历史账单 / 付费请求统计。
RTT 从 Hub 发出指令到收到对应 Bridge reply，以 Hub 的单调时钟测量，包含桥接传输与该指令处理时间；不是手机→站点→电脑的全链路延迟。
“最近在线时间”不当作延迟。单机“测量”发 `{type:"device.ping",nonce:"随机ASCII"}`，当前 Bridge 返回 `{nonce,receivedAtUnixMS}`；Hub 只校验 nonce，**不**相减双方时钟。旧版 Bridge 不支持或没有回复时明确测量失败；零样本显示未测量，不显示虚构 0 ms。

封禁立即撤销手机 token / 待用二维码并关闭电脑 / 手机 SSE，持久化按设备 ID 拒绝后续登记、stream、事件和指令；同一 ID 更换 secret 或内部命名空间不能绕过。
解封不会恢复旧手机 token，需要重新扫码。临时断开不是封禁：客户端可自动重连。
这些操作只处理远程连接，**不会终止电脑本机正在运行的 Codex / Claude 任务**，也不提供管理员 shell / 文件读取 / 任意命令执行能力。
随机设备 ID 不是硬件证明；恶意客户端创建新 ID 仍可重新登记，不能宣传不可绕过的用户 / 硬件封禁。不会封禁反代 IP，以免误伤同一代理后的其他用户。

修改类操作必须填写理由，由宿主页面再次确认并经过现有管理员 step-up 中间件。宿主 `step_up_enabled` 默认关闭：关闭时仍校验管理员资格，但不会要求额外 TOTP；启用后按宿主既有二次验证策略执行，并非强制每次输入 TOTP。审计记录时间、动作、匿名设备 ref、理由及宿主提供的管理员 ID（或明确的“宿主管理员”）；没有虚构用户名，不记录会话正文和原凭据。
管理页提示理由不得粘贴敏感内容；服务端拒绝常见 Key / Bearer / 64位十六进制凭证形态，但不能自动判断所有自然语言敏感信息，请运营者自行遵守隐私要求。

## 管理权限链与最小宿主扩展

管理动作 **不挂载在公网 Hub HTTP 路由上**。权限链为：宿主管理员会话 → 受 sandbox / source / bridge token 校验的插件 UI → 宿主 `/admin/plugins/:id/hub` 只读 API 或带 step-up 中间件的 `/admin/plugins/:id/hub/action` → Manager 校验指定已启用 Hub 插件 → 私有 Forward RPC。
宿主为私有请求设置 `ForwardRequestStart.account_id=-1`、`platform=salcara`、`account_type=http`；公开转发固定为零，任何 HTTP 请求头不能改变该 gRPC 元数据。
宿主 `startPluginRuntime` 必须开启 go-plugin `AutoMTLS:true`，SDK `Serve` 使用支持自动协商的 `DefaultGRPCServer`。每次进程启动生成临时双向证书，所有 RPC（包括 broker 反向服务）按证书校验；不能只依赖 localhost、MagicCookie 或二进制校验和，否则其他本机进程可伪造上述私有元数据。TLS 不保护已掌控宿主 OS 用户、进程内存或发布密钥的攻击者。
插件只在这组宿主私有元数据全部匹配时处理 `/salcara-hub/_admin/*`。公网 Manager 拒绝此路径，Hub 的公开路由永远没有该管理端点。没有新增能供手机 / 模型 Key 调用的 admin 接口，也没有修改 protobuf RPC / transport v1。

管理面在设备配对宿主扩展基础上，另需同步这些源文件：

- `backend/internal/service/plugin_hub.go`：私有 HubAdmin 转发与公开管理路径拒绝。
- `backend/internal/service/plugin_runtime.go`：Hub 独立持久化目录及所有插件 RPC 的 AutoMTLS（不可省略）。
- `backend/internal/handler/admin/plugin_hub_handler.go`：socket peer 覆写、管理列表 / 操作 API、操作者身份由宿主注入。
- `backend/internal/server/routes/admin.go`：管理员查询与 step-up 操作路由。
- `frontend/src/api/admin/plugins.ts`、`frontend/src/views/admin/PluginsView.vue`：宿主权限 API 与插件 UI bridge 消息；修改操作不能只靠 iframe 自己声称已确认。

原设备配对扩展还依赖路由装配、Hub capability 清单 / 包校验、Hub runtime 生命周期和数据目录、宿主插件页 / i18n；不能只复制上述管理文件就假装原版 Sub2API 已兼容。
当前 fork 的完整宿主适配源文件清单如下（不包含插件自身 `plugins/salcara-hub/**`）：

- 后端必需：`backend/internal/server/router.go`、`backend/internal/server/routes/admin.go`、`backend/internal/handler/admin/plugin_hub_handler.go`、`backend/internal/service/plugin_hub.go`、`backend/internal/service/plugin_manager.go`、`backend/internal/service/plugin_manifest.go`、`backend/internal/service/plugin_runtime.go`。
- 清单与安装校验：`backend/pkg/pluginapi/v1/manifest.schema.json`、`backend/internal/service/plugin_package.go`。后者的内置发布公钥是当前 fork 的独立发布选择，不代表本地未签包受信任；其他开源站点应使用自己的发布签名 / 信任配置，不能共享生产私钥。
- 宿主前端：`frontend/src/api/admin/plugins.ts`、`frontend/src/views/admin/PluginsView.vue`、`frontend/src/i18n/locales/zh/admin/plugins.ts`、`frontend/src/i18n/locales/en/admin/plugins.ts`。
- 回归测试：`backend/internal/handler/admin/plugin_hub_handler_test.go`、`backend/internal/server/routes/plugin_hub_admin_test.go`、`backend/internal/service/plugin_hub_admin_test.go`、`backend/internal/service/plugin_hub_integration_test.go`、`backend/internal/service/plugin_package_test.go`。
- 仅旧 API-Key 分组模式兼容：`backend/internal/server/routes/gateway.go` 的 `/v1/salcara-hub/key`；**新设备扫码模式不依赖这项补丁，可不移植**。

这是待人工审查的适配清单，不是已验证适用任意上游版本的一键补丁；本地宿主交付包只导出了白名单适配文件与基线 patch，并非整个 dirty fork。现有插件框架（数据库插件表 / installer / transport v1 / 管理员权限中间件）仍是前提，没有新增 protobuf 或数据库迁移。
当前目标是带本项目扩展的自定义 Sub2API `0.2.11+salcara.hub.1` + 插件 `0.3.1` / Hub `1.4.0`；真实公网部署、管理员 JWT / TOTP 实机交互尚需运营者验收。

本地测试覆盖公开路径 / 假 HTTP 头拒绝管理权限、宿主路由的管理员与 step-up 调用顺序、封禁与审计重启持久化、迟到 reply 在封禁 / 撤销 / 断开后不释放结果、以及管理请求参与旧 Hub 的 Close / 配置替换屏障。设置 `SALCARA_TEST_HUB_PLUGIN_PACKAGE=<本地未签名包>` 后运行后端 `go test ./internal/service -run TestSalcaraHubPluginRuntimeIntegration -count=1`，还会真实安装和启动该测试包，验证正常宿主 mTLS RPC / 私有管理操作 / 重载封禁，以及明文 gRPC 与无客户端证书 TLS 客户端均不能伪造管理员 RPC。该测试仅在临时目录允许未签名包，不修改生产信任配置。

## 本地打包与许可

配套 0.2.11 宿主插件卡片提供「检查更新」和「一键更新」，只在管理员手动点击时联网。Salcara Hub 默认使用独立公开仓库的 GitHub Release 更新清单；其他发布者可由站点管理员配置固定 HTTPS 更新源与发布者 ID。未配置、未信任公钥、尚未发布、无更高版本、宿主不兼容分别提示，不会假报有更新。
更新先校验大小 / SHA-256 / 发布者签名 / 文件哈希 / 插件 ID / 能力 / 版本与宿主兼容，再确认维护断连。运行中的原版本更新后可尝试恢复运行，原停用版本保持停用；保留加密配置及宿主插件数据，不删除手机配对记录。恢复失败会明确显示「已安装但未运行」，不承诺自动回滚。并发变更会拒绝更新并要求刷新。
按钮只更新插件本体，不给原版 Sub2API 添加宿主能力，也不会自动替换整个服务器。若官方宿主升级改变插件接口，仍需重新适配和管理员部署。更新清单不包含/授权新公钥；核对与信任公钥仍由站点管理员明确完成。
`tested_sub2api_versions` 的自定义版本记录来自本机临时目录中的真实安装器及 mTLS RPC 链路测试，不代表所有 Linux/反代/生产数据库和手机桌面实机组合都已经验收。

`go run ./tools/package -go <Go可执行文件> -output <明确输出路径>.s2plugin` 会本地构建 Linux amd64 / arm64、Windows amd64，打包管理员 UI、README、仓库 LICENSE 与清单文件哈希。
不传 `-signing-key` / `-key-id` 即为 **未签名测试包**，不能宣称受信任的正式发布；生产宿主可能拒绝未签名插件。
签名需要运营者自己的 Ed25519 发布密钥及宿主信任配置。`tools/keygen` 可在仓库外的全新私有目录生成密钥及独立公钥 JSON；Windows 写入前限制目录 ACL 为当前用户与 SYSTEM，Unix 限制为 0700。签名私钥绝不进入源码、GitHub、插件包或服务器。
本地签名包使用新发布者 ID `salcara-local-20260930`，不是现有内置信任 ID，也不代表 OpenAI 或 Sub2API 官方认证。运营者需把交付的公钥合并到 `plugins.trusted_publishers`，保留 `allow_unsigned:false`；不要覆盖整个已有配置。仅信任公钥还不能给旧宿主补上 Hub 能力。没有读取或修改既有生产私钥；公开发布以独立 `salcara-hub-plugin` 仓库中的实际 Release 为准，手机软件仍为私有测试构建。
本目录与对应宿主修改遵守仓库 `LICENSE` 所载 **LGPLv3** 及各依赖自己的许可，不把整个插件称作 MIT；桌面网关使用的 CLIProxyAPI 是另一组件，其 MIT 许可不替代这里的许可。
