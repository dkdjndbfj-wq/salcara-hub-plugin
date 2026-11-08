# 持久消息与图片存储：本地实施说明

2026-11-07。本说明对应本地源码实现，未发布或部署。原始需求与历史基线见 [设计文档](DURABLE-REMOTE-MAILBOX-DESIGN-20261107.zh.md)。

## 存储和生命周期

`DataDir/mailbox/mailbox.sqlite` 使用单连接写入、WAL 和 `synchronous=FULL`；首次建表放在一个事务，避免大量独立 fsync。事件、结果、离线指令、推送任务、图片分片和缩略图正文以 AES-GCM 加密。`key-v1` 与数据卷一起保存，`storageEpoch` 在正常重启后稳定；缺失密钥、密钥不匹配或 SQLite 完整性检查失败会阻止启动，不能静默生成替代密钥。备份/恢复应保持密钥、数据库及 WAL 的一致性。

| 数据 | 默认策略 |
| --- | --- |
| 未确认事件、结果 | 7 天，可设置 1–30 天 |
| 手机确认已持久保存的事件、结果 | 最多再保留 24 小时 |
| reasoning / message.delta 中间内容 | 24 小时 |
| uncertain 结果 | 第一次进入 uncertain 时开始计时，读取不续期 |
| 未完成图片上传 | 最后一次有效新分片后 1 小时 |
| 图片原图 | 最后一个真实消息/操作引用过期后额外保留 24 小时；会话读取关联本身不延长原图寿命 |
| 缩略图 | 7 天 LRU；绑定 32 MiB，全站 min(128 MiB, 总额度 5%) |
| 离线 start/send | 默认关闭；原 prepare 签发时间起最多 10 分钟，不续期 |
| 推送任务 | 最多 1 小时，每分钟重试；同一通知 ID 去重、发送前复查绑定与注册代次 |

默认全站额度为 min(5 GiB, 数据卷容量 10%)、每绑定 256 MiB。正文、原图、未完成分片、缩略图和离线队列统一计入绑定额度，数据库/WAL 的物理大小计入总额度。70% 提示、85% 触发到期清理；磁盘至少保留 max(2 GiB, 卷容量 5%)。后台每 5 分钟清理一轮，管理页可立即触发，每轮最多 500 个逻辑对象、50 MiB 正文；积压分轮处理，到期不表示物理空间在同一瞬间释放。图片先持久标记 deleting，再删除数据和元信息；重启可续清。容量不足时拒绝新图片/正文，不能提前删除仍受引用保护的图片。

每个回执最多保留 2 MiB 正文；按 turn ID（缺失时消息 ID，再缺失时会话）限制累计事件正文。超限留下 `event_body_truncated` 提示、`resetRequired` 缺口和任务终态，不能把截断内容称为完整历史。v1 无可信发送期限的身份 fence 保守保留；v2 执行回执元数据保留 30 天，原 client intent 的不可续签 fence 有界保留，满额拒绝新身份。

## 手机协议

能力协商增加 `commands.durable.v2`、`events.durable.v1`、`storage.blobs.v1`、`events.binding-scope.v1`。未协商 v2 的旧客户端保持原回执协议，旧 v1 待确认操作不得迁移为新服务器 UUID。

1. `POST /v1/app/commands/prepare`：`{deviceId, requestId:<原意图UUID>, command}`，只登记、不执行、不需要 sourceGrant。返回 `{operationId,ticket,dispatchNotAfter,storageEpoch,state}`。
2. 手机必须先保存 ticket 和原意图映射，再把 `command.operationId` 与 envelope requestId 换成服务器 UUID，并针对最终命令获取来源授权。既有 `/app/commands` 增加 `executionTicket`。
3. 相同原意图只返回原票据和原截止时间，过期不续签；已签发意图或服务端 UUID 不得退回无票据的 v1 派发。若原 UUID 已有 v1 回执，则返回 `legacy_operation_requires_recovery`。
4. `GET /v1/app/operations?deviceId&operationId&ticket` 只查询。state 为 prepared、queued、pending、completed、uncertain 或 expired。completed 的 `ok/result/error/code/httpStatus` 代表执行结果；其余状态不代表已执行。
5. `POST /v1/app/operations/ack`：`{deviceId,operationId,ticket}`。只能在手机将完整结果保存成功后调用。
6. `/app/events` 原分页增加 `storageEpoch`。`POST /app/events/ack`：`{deviceId,sessionKey,through,storageEpoch}`，手机事务保存页与游标后才确认。epoch 变化后从 0 恢复，保留本地已确认内容；到期/截断缺口返回 resetRequired 和仍保留的页。

所有 `dispatchNotAfter/retainedUntil/expiresAt` 为 Unix 毫秒。电脑离线时仍可读 Hub 保留页；没有独立的完整会话 snapshot 端点，缺口最终由原 Agent 历史补齐。离线队列只允许开放来源策略下的 session.start/send，不持久保存 sourceGrant。审批、打断、steer/continue、Git 写入不排队；来源策略或绑定改变会拒绝旧待发项。claimed 后崩溃视为 uncertain，不能盲目再执行。关闭离线队列后未派发项也会停止执行。

## 图片与缩略图协议

沿用 `att_[a-f0-9]{32}`。现有 `attachment.put` 的 index/total/data 分片由 Hub 持久接收，单片 Base64 不超过 200000 字符，最多 64 片，总原图不超过 8 MiB；逐片比较摘要，同 ID 不允许换内容。两个上传槽位，每次只解密/处理一片。整图完成使用真实 PNG/JPEG/WebP 头检查尺寸，超过 1600 万像素拒绝，原图接收不进行完整像素解码。最后一片已提交但 ready 标记尚未写入时重启，重传相同分片会续做最终校验。

`attachment.read` 继续按 sessionKey 授权，单片最多 90000 字节。增加 `variant:'thumbnail'`：返回原 att_ ID、JPEG 缩略图自己的 mime/size/sha256/width/height/data/offset/nextOffset/complete；原图引用不变，手机应分别保存 thumbnailUri 与原图 URI。没有可用缩略图时返回明确失败，不悄悄返回原图。

原始流接口：`POST /v1/app/blobs?deviceId&attachmentId`、`POST /v1/bridge/blobs?deviceId&attachmentId`，Content-Type 为 image/png、image/jpeg 或 image/webp；GET 使用 `?deviceId&id`，可加 `variant=thumbnail`。相同 ID、相同字节可重试。

缩略图长边 512、单解码工作槽、JPEG 输出；8-bit 原图最大 16 MP，预览不超过 512×512。16-bit PNG 允许保存原图，但拒绝派生缩略图以避免突破解码预算。96 MiB 是代码构造上的工作预算，不是已完成的容器 RSS 实测。

## Bot 换绑和来源隔离

Bridge 注册响应、SSE hello 和心跳提供 `eventScope` 与 `eventScopeRequired`（Bot 为 true）；hello 在 pair.status 和命令之前发送。scope 在普通重启后不改变，手机绑定代次变化会改变。命令 envelope 也携带该 scope。Bridge 必须在冻结事件批次时保存原 scope，并在图片请求使用 `X-Salcara-Event-Scope`。

Bot 模式的事件或图片请求缺少 scope 或 scope 不匹配，一律返回 409 `event_scope_changed`；旧客户端不能靠兼容路径把旧离线批次转投给新手机。换绑会关闭旧 Bridge 连接并要求重新握手。QR 模式继续严格校验已有 bindingId/phoneHash；提供 eventScope 时也必须匹配。已经保留的事件/图片按绑定及 credential generation 隔离。

## 管理和验证

独立管理页“系统 → 消息与图片存储”可设置保留天数、全站/绑定容量、离线队列，查看占用和磁盘预留不足状态，并执行到期清理。管理 API 为已有管理员会话/CSRF 保护下的 `/_admin/v1/storage`。没有向普通手机公开管理接口。

主要实现：`internal/hub/durable_*.go`、`command_receipts.go`、`server.go`、`event_page.go`；管理代码在 `internal/standalone/durable_storage.go` 与 `web/app.js` / `index.html`。第三方依赖许可随 `licenses/durable/` 和 Docker 镜像保存。

本地验证命令：

```text
go test ./internal/hub ./internal/standalone -count=1 -timeout=300s
go test -race ./internal/hub -run 'TestDurable|TestReceiptConcurrent|TestBridgeEventBatch' -count=1 -timeout=120s
node --test deploy/standalone/admin-ui.test.mjs
```

本轮实际结果：`go test ./... -count=1 -timeout=300s` 全包通过（Hub 157.624s、standalone 45.328s）；随后最后分片崩溃窗口修复及图片/缩略图/Bot/并发回执/事件批次的针对性 race 测试通过（6.321s）。完整 durable/并发回执/批次 race 此前通过（11.635s），管理页测试 23/23 通过。`git diff --check` 无空白错误（仓库已有 CRLF 提示）。

新增测试涵盖结果/事件重启恢复、加密内容不泄漏、密钥缺失失败、批次去重、旧代次隔离、过期缺口、固定票据、防 v1 降级、离线队列一次派发、claimed 不重放、图片中断续传/内容冲突/共享引用/读取租约、缩略图分页/会话隔离/禁止原图回退/清理、40 分片受限来源上传不耗尽 32 个授权槽位、磁盘保护和清理共享预算。

尚未做真实 384 MiB 容器峰值、宿主断电/磁盘故障注入、真实 Android 厂商后台杀进程与 FCM 到达率验收。设备进程被系统强杀时无法保证实时收到推送；恢复依靠持久信箱。Docker 日志现状为 10 MiB×3 容量轮转，并非按 7 天定时删除。多 Hub 实例不能共享同一 SQLite 文件；没有宣称外部 Agent 动作 exactly-once。
