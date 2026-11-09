# 远程发现、设备身份与入口别名

本契约对应声明 `discovery.policy.v1` 的 Hub 源码。客户端需要配套实现；本文件不表示新版本已发布或线上已部署。公开网页只展示服务状态，不用于验证模型 API Key。

## 匿名服务发现

`GET <hub-api>/ping` 保留 `service`、`protocol`、`protocolVersion`、`capabilities`、`desktopCompatibility` 等字段。它忽略 Authorization、Cookie、设备密钥、登记码、手机配对和 Bot 令牌，不访问模型鉴权服务，也不消耗身份验证失败额度。

`remoteAccess` 仅为全站公开策略。即使请求携带有效身份，也不会返回用户例外。旧设备凭证不影响健康 Hub 返回 200；停止中的 Hub 返回 503 和 `code: remote_unavailable`。独立部署的包装层保留底层 4xx/5xx 状态、错误码和响应头，不把设备拒绝改写成服务故障。

## 只填写 API 地址时的发现范围

普通用户继续只填写模型 API 地址和 Key。客户端只取地址生成匿名候选，不要求补填 Hub 状态页地址，不读取状态页 HTML，也不向候选发送模型 Key。独立 Docker Hub 的默认公开发现端点为 `/salcara-hub/v1/ping`。

配套 Bridge 的 `DiscoveryTargets(apiURL, "")` 按以下顺序探测；表内均为同一 origin 下的实际 GET 路径：

| 已保存的 API 地址 | 按顺序探测的路径 |
| --- | --- |
| `https://api.example` | `/salcara-hub/v1/ping` |
| `https://api.example/v1` | `/salcara-hub/v1/ping`，`/v1/ping` |
| `https://api.example/team` 或 `/team/v1` | `/team/salcara-hub/v1/ping`，`/salcara-hub/v1/ping`，`/team/v1/ping` |

只有前一个候选返回 404 才继续；403、503、重定向、超时或不兼容的成功响应不会触发路径搜索。最多三个候选，不枚举其他目录。最后一个候选支持 Hub 直接挂载在已提供的 API 路径，但仍要求返回完整 Hub 协议。已有内部连接地址恰好以 `/salcara-hub` 或 `/salcara-hub/v1` 结尾时只探测该入口；这属于兼容行为，不要求用户把该地址填成模型 API 地址。

支持完整自定义入口路径，并不代表只凭 origin 就能发现任意未知路径。例如用户保存 `https://api.example`，Hub 只部署在 `/private/remote/v1`，且没有上述公开入口或已有受信连接时，本契约不能自动找到它。运营方需提供约定的公开发现入口，或使 Hub 挂载与实际 API 路径约定一致；客户端不能让普通用户改填状态页来补救。本轮没有新增 well-known 发现端点或修改代理配置。

签名 ingress 清单在**已经找到 Hub、能读取清单之后**提供完整自定义入口列表，再通过固定身份和新 nonce 证明验证别名。它不解决首次定位清单自身所在的未知路径。内部显式 `override` 或已保存入口可以精确访问自定义路径；`Discover(alias, alias)` 仅证明这种已知地址流程，不能作为 `Discover(apiURL, "")` 首次发现的验收证据。客户端需要分别验证候选顺序/404 边界与签名别名流程。

## 有效用户策略

声明 `discovery.policy.v1` 后，客户端通过 `GET <hub-api>/discovery/policy` 读取有效策略，使用下面一种身份：

- 已登记电脑：`X-Salcara-Device-Id` 与 `X-Salcara-Device-Secret`。
- 首次登记预览：`X-Salcara-Enrollment-Claim`。读取不会消耗登记码。

拒绝混合或重复凭证、模型 Bearer、手机/Bot 令牌和 URL 查询参数。有效凭证不受其他用户在共享 IP 上触发的失败限流影响；无效身份仍计入现有失败限流。

```json
{"ok":true,"scope":"device","remoteAccess":{"mode":"restricted","revision":"...","issuerCount":1,"station":{}}}
```

`scope` 为 `device` 或 `enrollment`。用户例外只由验证成功的身份决定，保留用户策略 revision。未登记、被禁用或凭证失效时不会返回全站策略作为替代。客户端在该接口失败时必须停止使用旧的有效策略，不能回退到 ping 的全站策略；普通模型 API 配置和调用不受该远程状态影响。

| HTTP | code | 含义 |
| --- | --- | --- |
| 401 | `device_credentials_required` | 缺少设备身份；不能据此判断模型 Key 无效 |
| 403 | `device_credentials_invalid` | 设备身份缺失、不匹配、过期或撤销 |
| 403 | `device_enrollment_required` | 新设备注册缺少登记码 |
| 403 | `enrollment_claim_invalid` | 登记码错误、过期、已用或不适用 |
| 403 | `pair_credentials_invalid` | 手机配对身份失效 |
| 403 | `bot_credentials_invalid` | Bot 身份失效 |
| 423 | `user_disabled` | 管理员禁用用户 |
| 400 | `ambiguous_credentials` / `invalid_request` | 混合身份或非法请求 |
| 429 | `remote_rate_limited` / `device_capacity_exceeded` | 频率或设备容量限制 |
| 503 | `remote_unavailable` | 服务停止或不可用 |

原有 `error` 字段保留；已有 `source_*` 授权错误保持原义。旧版不返回 code 时，客户端按请求所属操作和 HTTP 状态分类，不能把任意远程 403 归为模型 Key 失效。显式开启旧 API-Key 模式的安装继续兼容旧注册协议，其 Bearer 和附属设备密钥仍分别验证。

## 签名入口清单

配置入口身份的 Hub 另声明 `ingress.signed-manifest.v1`。`GET <hub-api>/ingress/manifest` 保留 version 1 的平铺字段，增加标准带填充 base64 字段 `signed_payload`、`signature`。

`signed_payload` 解码为原有八个字段的精确 UTF-8 JSON：`version`、`hub_instance_id`、`identity_public_key`、`canonical_url`、`revision`、`issued_at`、`expires_at`、`endpoints`。其中不包含两个签名封装字段。Ed25519 签名消息为：

```text
UTF8("salcara-ingress-manifest-v1\n") || BASE64_DECODE(signed_payload)
```

不得重新序列化 JSON 后验签。客户端使用已固定的公钥验证签名，核对 Hub ID、公钥、正 revision、有效时间及完整候选 API URL 的精确成员关系；只使用签名载荷中的值，或要求平铺字段与其完全一致。然后向该入口请求一次新的 nonce 身份证明，验证成功后才能发送设备或登记凭证。

仅验证 nonce 证明不足以信任候选入口：转发代理可以篡改未签名列表，并把挑战转发给真正 Hub。旧的未签名清单只能从原来已信任的 HTTPS 地址读取，不能靠候选入口自己提供的未签名列表建立别名。初次身份固定、无签名旧 Hub 的精确地址兼容和重置后的重新登记由客户端处理，不能合并不同实例。

清单只发布 canonical 和启用且验证成功的入口，最多八项，有效期 24 小时，响应 `Cache-Control: no-store`。原有 nonce 证明格式和 30 秒有效期保持。正常重启保留实例身份；更换/清空数据目录产生新身份，旧设备绑定失效。入口别名不增加任何模型 API、外站或反代来源授权。

## 路径规则

入口必须使用现有公网 HTTPS 主机规则。保留完整路径和路径大小写：

- 无路径根地址规范化为 `/salcara-hub/v1`。
- `/<prefix>/salcara-hub`（可带结尾 `/`）补 `/v1`。
- 显式 `/<prefix>/v1`，包括 `/v1`，保持该 API 挂载路径。
- 段只允许 ASCII `[A-Za-z0-9._~-]+`，拒绝单独 `.` / `..`、任何 `%`、重复 `/`、反斜线、用户信息、查询和片段。

不清理穿越路径，不通过同域、域名后缀或 origin 相等推断别名。显式 `/salcara-hub.evil/v1` 可以是自定义挂载；`/salcara-hub.evil` 本身不是 Hub 根地址。

插件 `Config.Prefix` 与对应 `PublicURL` 组合保留完整挂载路径。根挂载 `Prefix: "/"` 的公开根地址使用 `/v1`，已提供的 API 路径不重复追加。独立 Docker Hub 仍服务固定 `/salcara-hub`；外部带前缀入口依赖已有代理路由，本次没有修改代理或 Nginx。

## 本地回归

`discovery*_test.go` 验证匿名一致性、用户例外隔离、非消耗登记预览、重启、HTTP 真实回环连接及服务重置分类；`device_auth_errors_test.go` 验证错误分类和旧注册兼容；`ingress_path_test.go`、`ingress_manifest_test.go` 验证路径、签名篡改、发布过滤、身份持久化及来源授权隔离。客户端仍需独立验证凭证不跨 Hub 泄露、切换时的迟到响应和普通 API 可用性。
