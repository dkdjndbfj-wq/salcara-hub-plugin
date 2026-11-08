# 远程 API 来源与访问策略设计

2026-11-08 更新：全站分类授权已实现 StationPolicy v2，实际配置、版本迁移与信任边界以 [全站远程使用授权](REMOTE-SOURCE-AUTHORIZATION.zh.md) 为准。下文保留原始设计背景。

状态：运行时内核已接入（默认 `open` 保持兼容）；`restricted` 需要配置签名发行方策略文件，未配置时 fail-closed。Hub 会把非秘密来源合同转发给 Bridge，Bridge 还会在任务锁内复核 API handle、来源地址、Agent、控制面和 API 指纹；仍需在部署环境完成真实双站验收。

本文解决一个容易被混淆的问题：电脑已经安装 Codex / Claude CLI，或者手机已经扫码绑定，并不代表这台电脑当前使用的任意 API 都可以被手机远程调用。远程控制必须同时满足“设备已配对、Agent 能力存在、当前凭据来源获准”三个条件。

## 1. 当前问题与边界

当前独立 Hub 的配对边界仍是“已配对且未封禁的设备”，来源策略则由 `remoteAccess.mode` 另外控制。Hub 不保存 API Key；生产 Bridge 还要求远程命令携带可解析的 Salcara API handle，不能仅凭 CLI 存在、原生配置或第三方反代启动状态执行。

因此会出现以下风险：

- 只要电脑被配对，任意本地 API 条目可能被当成可远程使用；
- 检测到 CLI 只能说明程序存在，不能证明正在运行的会话使用了哪一把 Key；
- “本站 URL”“模型名称”或 `/models` 返回值都不能证明账号属于本站；
- 切换 A 站到 B 站时，如果只切换展示项，可能把 A 站的授权误带到 B 站；
- 只在手机界面拦截不能防止直接伪造 Hub HTTP 请求。

配对、连接、读取历史和执行任务应当分层。一个策略失效或被撤销，也不能删除电脑本地历史，更不能无故终止正在执行的本地任务。

## 2. 术语：四个对象必须分开

| 对象 | 含义 | 是否等于远程授权 |
| --- | --- | --- |
| Pairing | 手机与某台电脑在某个 Hub 上的设备绑定 | 否 |
| Control surface | Codex/Claude 的 CLI、桌面控制面或只读会话接口 | 否，只表示能力边界 |
| Credential source | 当前任务实际使用的 API、官方登录态或反代账号来源 | 是授权判断的输入 |
| Policy decision | Hub 根据来源、发行方、Agent 和版本得出的允许/拒绝结果 | 是最终结果 |

`source` 不能用当前 `ApiSource`（手机选择/电脑选择/工具默认）代替；`agent` 也不能只用 `codex`、`claude` 字符串代替。执行快照至少要包含 `hub + device + binding + agent + controlSurface + credentialHandle + providerOrigin + policyRevision`。

## 3. Hub 的三种策略模式

建议增加一个明确的 `remoteAccess.mode`，而不是把资源模式 `economy/balanced/performance` 当成权限开关：

### `disabled`

- 可以发现 Hub、扫码配对、查看设备和有限连接状态；
- 不允许新的远程 `session.start`、`session.send`、远程审批或跨站交接；
- 已在电脑上运行的任务不被强行杀掉；手机仍可按策略读取其终态/错误或执行安全的中断；
- 历史数据不因禁用而删除。

### `restricted`

只允许管理员明确开启的来源类别，建议初始类别为：

- `site_api`：本站发行方签发、且绑定到本站部署的 API；
- `verified_proxy`：通过可信发行方验证的反代账号；
- `official_account_proxy`：未来 GPT 等官方账号反代连接器，必须单独注册能力，不能自动归入 `verified_proxy`。

普通本地 `api` 配置、CLI 默认登录态、仅凭 URL 相同的反代、未知 Agent 均拒绝执行。仍可以返回“未获准”的原因码，不返回 Key、上游响应正文或敏感指纹。

### `open`

允许电脑端已配置且 Agent 实际能够使用的来源，但仍必须满足设备已配对、未封禁、命令签名/回执正确和能力检查。`open` 不是匿名远程，也不绕过 Hub 的设备认证。

现有独立 Hub 的行为等价于 `open`。升级时不能悄悄切到 `restricted` 造成用户任务全部失败；应在迁移中明确显示当前模式，要求管理员主动切换。新部署可以根据是否配置发行方验证器给出建议，但不能宣传为已经完成来源校验。

## 3A. 从 Cockpit Tools 和 Sub2API 借鉴什么

这两个项目解决的是不同层面的问题，不能简单合并成“检测到 CLI 就放行”：

- Cockpit Tools 的 Codex API Service 在本机建立经过认证的 OpenAI 兼容网关，把选定的 Codex profile、OAuth 账号和 API Key 账号生成到 sidecar 的配置/manifest，再由网关做账号选择。它把 `auth_mode`、`api_provider_id`、`api_base_url`、模型目录/映射等信息分开保存，并提供会话粘性、配额/健康冷却、模型允许/排除和账号池路由。参考：[架构交接文档](https://github.com/jlcodes99/cockpit-tools/blob/main/docs/CODEX_API_SERVICE_HANDOFF.md)、[Codex 账号模型](https://github.com/jlcodes99/cockpit-tools/blob/main/src/types/codex.ts)。
- Sub2API 把“上游账号”和“用户 API Key”分成两层：上游账号可以是 OAuth 或 API Key；用户 Key 再绑定分组，由分组决定模型、配额、并发和可用范围。它还用 composite group 做显式的模型到提供方路由，未知或含糊的模型会失败关闭，而不是猜一个提供方。参考：[项目说明](https://github.com/Wei-Shaw/sub2api#readme)、[Composite Groups](https://github.com/Wei-Shaw/sub2api/blob/main/docs/COMPOSITE_GROUPS.md)、[API Key 鉴权](https://github.com/Wei-Shaw/sub2api/blob/main/backend/internal/server/middleware/api_key_auth.go)。

Salcara 应采用相同的分层，但把远程控制授权独立出来：

```text
ProviderAdapter      上游协议/来源验证器（Sub2API、Cockpit、官方账号反代）
    ↓
AccountRecord        OAuth / API Key / 反代 / 官方账号，凭据仍留在实际执行端
    ↓
RouteGroup           模型目录、别名、精确/前缀路由、能力和版本
    ↓
SessionBinding       当前会话固定的来源、路由版本、模型和站点
    ↓
EligibilityGrant     Hub 可验证的远程来源授权
```

其中：

- 账号池的调度、配额、健康和故障转移属于 ProviderAdapter；它们不是 Hub 的权限判断；
- `publicModel → upstreamModel` 必须有可审计的 route version，精确匹配优先于前缀匹配，未知模型在 `restricted` 下拒绝；
- 新会话可以使用新的模型组，正在运行的会话固定 `SessionBinding`，避免中途换提供方或换协议；
- `site_api` 可以由 Sub2API 的分组/账号验证器签发 grant；Cockpit 本地网关需要显式注册的本地 connector，不能因为 URL 看起来相同就获得本站资格；
- 计费、额度和账号健康只影响“是否可调度”，来源证明和 Hub 策略才决定“是否可远程执行”。

这能同时兼容 Cockpit 的“电脑本地多账号切换”和 Sub2API 的“服务器账号池/分组路由”，又不会让 Salcara Hub 变成第二套模型网关。

## 4. 来源证明：不能把 URL 当作证明

### 4.1 推荐的证明对象

可信发行方（本站 API 服务、受信任的反代服务或未来 GPT 账号连接器）签发短期、最小范围的 `eligibilityGrant`。Hub 只验证签名，不接收 API Key：

```text
version, issuer, audience
deviceId, bindingId, credentialHandle, agent, controlSurface
sourceType, origin, configRevision
policyRevision, model, requestId, commandHash
issuedAt, expiresAt
```

公钥固定在 Hub 部署配置或发行方目录中；私钥不进仓库、不进 Docker 镜像、不进手机。`credentialHandle` 必须是电脑端生成的不可逆、不含 Key 的句柄。Hub 不保存明文 Key，也不把 Key 放入 receipt、事件、日志或推送。

### 4.2 两类“自建反代”要分开

用户所说的“自己反代的账号”可能有两种含义，不能用一个宽泛开关混在一起：

1. **中转站提供的反代账号**：由中转站的发行方签发 `verified_proxy` grant，Hub 信任该发行方公钥。
2. **用户自己的官方账号反代**：由用户配置的反代连接器签发专用 grant；没有可验证发行方时，在 `restricted` 模式中默认拒绝，不能因为本机存在登录文件就自动放行。

只填写 URL、只返回一个正常 `/models` 列表、或把名称写成 `Salcara` 都不是证明。开源的电脑端可被修改，因此签名 grant 只能证明“已获准的来源声明”，不能宣称能密码学证明每一个本地进程永远没有换 Key。若要强制每次推理都经过本站，只能让任务进入本站/可信 Worker 网关，由网关掌握真正的上游凭据；这属于更强的托管模式，不应伪装成普通本地 API 模式。

当前独立 Hub 已提供 Ed25519 来源证明合同。通过 `SALCARA_HUB_REMOTE_ACCESS_POLICY_FILE` 指向策略 JSON；只有签名发行方、来源类别、设备绑定、Agent、控制面、模型、账号句柄、requestId 和命令哈希全部匹配才允许执行。没有验证器时，`restricted` 返回 `source_policy_unavailable`，而不是失败后回退为 `open`。默认 `open` 仍保持旧部署兼容。Bridge 已接收非秘密 source envelope，并在配置任务锁内二次复核；`ConfigRevision` 约定为当前 API 卡片的 `AccountFingerprint`。这仍不是操作系统级进程签名证明，无法证明同一用户权限下的恶意进程未篡改内存。

## 5. 授权检查放在哪里

### Hub（权威边界）

以下入口都要在入队前检查同一个 policy snapshot，并把 `policyRevision` 写入回执：

- `session.start`、`session.send`、远程审批/问题回答；
- `interrupt`、`openOnDesktop` 以及跨站 `station.switch`；
- command receipt 首次创建和重复回放；
- 事件、历史、附件引用只做相应的读取/会话范围检查，不把读取权限误当成执行权限；
- 未知的新命令在 `restricted` 下默认拒绝，直到声明它的操作类别。

实现上，新 receipt 在持久化前和真正写入 Bridge 连接前各做一次策略检查；策略切换使用独立读写门锁住这两个短临界区，但不会持锁等待 Agent 回复。已发送的任务不会因切换被终止；同 owner/hash 的已完成缓存回执仍可恢复。

回执重放必须重新验证设备身份和回执所有者，但同一个成功回执不能因为缓存失效而重复执行。来源策略关闭后，新的 start/send/approval 拒绝；已经运行的任务继续本地执行，手机可以读取其进度/最终结果，并保留安全中断路径。封禁或解除配对是更高等级的安全动作，可立即切断远程传输，但不删除本地任务。

### Desktop Bridge（执行前第二道检查）

Hub 通过后，Bridge 在真正启动 Agent 或发送消息前再次检查：

1. 当前 `credentialHandle`、origin、adapter、Agent 和 control surface；
2. 本地 API 卡片指纹与 Hub 给出的 `configRevision/policyRevision`；
3. 当前任务是否仍使用同一份不可变执行快照。

检查必须在已有 `taskConfigMu` 的同一事务边界内完成。不能因为手机只换了 API 展示项就切换正在执行的任务；换站或换来源应先冻结新的发送，再准备目标 grant，成功后原子提交，失败则继续使用 A 站。

### Mobile（体验层，不是安全边界）

手机只展示 Hub 返回的结构化 decision：`allowed / denied / pending / stale` 及稳定原因码。它可以隐藏不允许的选项，但不能依赖前端拦截来保证安全。缓存键必须包含 Hub、设备、绑定、凭据句柄、来源和策略版本；切换 A→B 后不能复用 A 的允许结果。

## 6. A 站切换到 B 站的事务

目标站点与目标 API 必须一起切换，不能先改 Key 再补服务器：

1. 手机选择 B 站和 B 站 credential handle；
2. 电脑向 B 站验证设备/来源 grant，并取得目标 `policyRevision`；
3. Bridge 冻结新发送，但继续接收当前任务事件；
4. Hub B 校验 pairing、Agent 能力、来源类别和模型/协议；
5. 电脑原子写入 B 的 Applied binding，并返回带目标摘要的持久化 receipt；
6. 成功后使 A 的授权缓存失效，恢复队列；任何一步失败都保留 A，不产生“Key 已换、Hub 仍在 A”的半状态。

目标站点不可达时不应循环重试或把请求转发到错误站点。交接回执必须带站点、binding、credential handle 和配置版本，不能只带模型名。

## 7. 性能和算法约束

- 将允许来源按 `issuer + normalizedOrigin + adapterId + sourceType` 建索引；单次策略判断为 O(1)，Agent/来源目录构建为 O(n)。
- grant 验证按 `grantId/configRevision` 做 singleflight，只在配对、执行、切站或即将过期时验证，不做持续轮询。
- 缓存必须有短 TTL、明确的 policy/config revision 和上限；策略变更、Key/URL/协议/登录态变更立即失效。验证器超时、签名错误、目录版本不兼容均 fail-closed，不得回退为 `open`。
- 对来源目录、grant、模型列表、响应体、事件和附件都设数量/大小/时间上限；日志只记录匿名 operation ID 和原因码。
- 允许策略的读取路径与执行路径分开，避免手机打开会话时触发模型调用或重复验证上游额度。
- 单飞和请求取消不能阻塞历史读取；连续空长轮询要有最小退避，避免代理返回空响应造成紧循环。

## 8. 兼容和迁移

旧 Bridge 没有 capability、来源描述或 grant 时：

- `open` 模式可使用明确的兼容路径，并在诊断中标记“未验证来源”；
- `restricted` 模式拒绝执行，只允许配对、状态和有限历史读取；
- 不从旧的 `remoteSendSupported`、CLI 探测结果或手机缓存推导允许；
- 兼容路径有独立版本号和过期日期，不能永久隐藏安全降级。

新增来源类型（例如 GPT 账号反代、Hermes）只新增适配器和验证器，不复制一套 Hub/手机协议。适配器必须声明 Agent、控制面、来源类型、附件能力、审批/问题能力和 grant audience。

## 9. 必须通过的验收矩阵

至少覆盖：

1. 本站 Key、可信反代、用户未验证反代、官方登录态、普通本地 API 在三种模式下的允许/拒绝；
2. URL 前缀伪造、大小写/默认端口差异、错误 issuer、过期/重放/错误 audience 的 grant；
3. 电脑离线、Hub 重启、响应丢失、receipt 重放、手机杀进程；
4. A→B 站切换在每一个步骤失败时仍保持 A 的一致状态；
5. 策略关闭时正在运行、排队、等待审批和正在上传附件的任务；
6. Codex CLI、Codex 桌面控制面、Claude Code、Claude Desktop 只读会话和未知 Agent；
7. 直接绕过手机调用 Hub、伪造 capability、旧 Bridge/旧手机版本；
8. 日志、崩溃、备份、推送和缓存中不出现 API Key、pair token 或 grant 私密材料。

必须增加真实的 Hub 重启、电脑进程重启、反向代理超时、A→B 双站和 Android 后台验收；单元测试通过不能代替这些链路验收。

## 10. 实施顺序

1. 先冻结协议：`credentialHandle`、来源枚举、`policyDecision`、`policyRevision`、grant schema 和错误码（已落地 `SourceSnapshot` / `SignedSourceGrant`）；
2. 在 Hub 增加只读策略评估器和审计日志，不改变现有 `open` 行为；
3. 接入发行方公钥/签名验证器，覆盖 site API 与 verified proxy；
4. 在 Bridge 的实际 start/send/approval/switch 前加入第二道快照检查；
5. 加入 receipt、事件、附件和历史的策略边界及失效处理；
6. 手机只消费 decision 和原因码，保留现有 UI，不把安全判断放到 UI；
7. 做上述故障矩阵后，才在 Hub 管理界面公开三种策略开关和迁移提示。

在第 3 步之前，不能声称“本站 API 已限制”或“自己的反代账号已验证”。在第 4 步之前，Hub 的 allow decision 仍不能证明电脑真正使用了获准来源。

### 当前运行时错误码

受限策略的拒绝均为 HTTP 403、`retryable=false`，包括：`source_policy_unavailable`、`source_grant_required`、`source_grant_invalid`、`source_grant_expired`、`source_policy_stale`、`source_command_mismatch`、`source_credential_mismatch`、`source_agent_mismatch`、`source_agent_denied`、`source_model_mismatch`、`source_control_surface_mismatch`、`remote_execution_disabled`。旧版手机/Bridge 未提交 `accountId` 和签名 grant 时会被拒绝，这是故意的 fail-closed 行为；`open` 模式不受影响。
