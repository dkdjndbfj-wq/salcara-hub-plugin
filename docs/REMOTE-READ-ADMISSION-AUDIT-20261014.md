# 远程流与手机长轮询准入修复（2026-10-14）

## 范围与结论

本轮只修改 Hub 的 App / Bridge 读取并发准入，不改管理台布局、手机或桌面 UI，不发布、不部署。代码来自已有未提交工作树；下列范围不代表这些文件里的全部历史修改均由本轮完成。

发现：`/app/stream` 已受每 owner 和全局连接预算限制，但 `/app/events?wait=25` 的空读取可无限进入等待。已配对但失控的客户端可绕过三种资源模式的读连接预算，占用请求协程、计时器与连接。

修复后，SSE 和真正需要等待的空事件读取共享同一预算。已就绪的事件页、需要重建快照的 reset 响应、`wait=0` 的即时读取不额外占等待名额。

发布门禁追加复核发现，原 SSE 预算按可投递订阅表长度计算；队列溢出、撤绑或删除账号时，订阅已经移除，但网络 Write/Flush 可能尚未退出，提前释放的名额仍可被重占。最终版本同时将 SSE 改为活跃 handler 计数：保留订阅表用于消息投递，准入计数仅在 handler 真正退出时释放。不把这一已知绕过仅作为未修复边界保留。

随后同一资源契约检查确认 Bridge SSE 也有实路径：同一设备连续重连可替换 `dev.conn`，旧 handler 即使未退出也只算一个 registry 项，而且替换连接原本豁免全局检查。最终一并修复 Bridge 的独立全局预算，按真实活跃 handler 计数，不与 App 预算互相挤占。

## 文件归属

- `internal/hub/server.go`：App SSE / 长轮询共享准入与总计数；满额返回 HTTP 429、`app_read_capacity`、`retryable: true`、`Retry-After: 2`；Bridge 独立活跃总计数，满额返回既有 429 错误与 `Retry-After: 2`；各自 handler defer 释放。
- `internal/hub/store.go`：每 owner 活跃 SSE / 长轮询计数，受现有 Hub mutex 保护。
- `internal/hub/resource_mode.go`：共享预算检查 helper；App / Bridge SSE 全局计数从扫描 registry 改为 O(1) 活跃计数；保留三种模式的既有数字，模式切换不取消已经接受的读请求。
- `internal/hub/event_wait_admission_test.go`：新增长轮询及混合传输回归；SSE 配额 fixture 经真实 ServeHTTP 准入，不再用人工 seed 订阅表代替请求。
- `internal/hub/stream_admission_test.go`：新增支持 ResponseController / 写截止时间的合成阻塞 writer 与 SSE drain 回归。
- `internal/hub/bridge_stream_admission_test.go`：新增 Bridge 替换、满额重连、生命周期、模式与并发冲击回归；复用真实 ServeHTTP 和可控网络阻塞 fixture。

释放使用开始等待时的原 account 指针，不使用醒来后的可变 account 变量。账号/设备删除、重新配对、相同 owner 重建、模式切换不重置计数；旧请求只释放自己占用的名额，全局计数仍包含正在退出的旧 owner。

## 第一阶段：长轮询反例与验证

反例 `TestLongPollAdmissionSharesExistingSSEOwnerBudget`：填满同 owner 的 SSE 名额后发起空长轮询，原实现仍等待并返回 200，测试失败（包耗时 1.532s）。最小修复后返回 429，测试通过（0.578s）。

新增回归覆盖：

1. SSE 已满时长轮询被拒绝；长轮询已满时 SSE 也被拒绝。
2. 单 owner 满额不影响另一 owner 尚有预算的读取；释放名额后可以重新进入。
3. 两种 transport 共享全局预算，不各自获得一套额度。
4. 12 组独立合成配对同时发出 96 次读取，economy 最多保留 32 个空等待，每 owner 最多 4 个。
5. 满额期间即时读取、就绪事件页及 reset 响应仍可使用。
6. 取消、自然超时、新事件、撤绑、重新配对、Hub 关闭、account 删除重建均释放原名额。
7. 降低资源模式不取消已有读取；后续准入按新限制，释放及再提高模式后计数正常。

命令（使用项目既有 Go 1.27.1 / Windows 工具链，无额外依赖）：

```text
go test ./internal/hub -run '^TestLongPollAdmission|^TestAppEventsLongPoll|^TestConcurrentUsers|^TestOneUsersFullCommandQueue|^TestLiveResourceTrim|^TestGlobalCacheBudget|^TestModeChangeDoesNotCancel' -count=1 -timeout=2m
```

结果：通过，3.979s。涵盖本轮新增以及既有长轮询、32 用户隔离、命令队列、资源裁剪回归。

相同定向集合 `-race -count=3 -timeout=3m`：三轮通过，23.515s，无 race 报告。本记录不代替根任务随后运行的全包验收。

## 第二阶段：App SSE drain 反例与验证

反例 `TestSSEAdmissionDroppedStreamReservesOwnerUntilDrain/write/queue-bytes`：一个 SSE 已获准且阻塞在 Write，队列溢出使其从订阅表移除后，同 owner 的新 SSE 仍获准返回 200，先红失败（包耗时 2.448s）。最终修复后新 SSE / 长轮询均返回 429，只有原 handler 真正退出后才能重占名额。

追加覆盖：

1. Write 与 Flush 两类阻塞，队列字节溢出、队列消息溢出、撤绑再配对均不提前释放名额。
2. 已删除 account 的阻塞请求仍占全局预算；相同 ID 重建后，旧 defer 只释放旧 account，不能给新 account 扣成负数或串到另一 owner。
3. 正常完成写入和写截止错误都只释放一次；重复取消旧请求不影响新请求。
4. 保留每次写入的 30 秒 deadline；测试用 ResponseController 缩短至过去时间确定性模拟超时，不等待真实网络。
5. Hub shutdown 等待已获准的阻塞 handler，在 deadline 解除阻塞后完整退出；shutdown 后不再接收新请求。
6. 12 组独立合成配对共 98 次 SSE / 长轮询混合读取（其中 2 次预先获准以确保确实混合），economy 的活跃合计最多 32、每 owner 最多 4，全部退出后 owner/global 计数归零。
7. 5 个已从订阅表移除但未 drain 的 SSE，在 performance 降到 economy 后继续计数，释放后才重新准入；再升到 balanced 不重置计数。

最终相同源码的定向集合：

```text
go test ./internal/hub -run '^TestSSEAdmission|^TestLongPollAdmission|^TestAppEventsLongPoll|^TestConcurrentUsers|^TestOneUsersFullCommandQueue|^TestLiveResourceTrim|^TestGlobalCacheBudget|^TestModeChangeDoesNotCancel|^TestAdmin|^TestQRSingleUse' -count=1 -timeout=2m
```

普通通过 **4.050s**；同集合加 `-race -count=3 -timeout=3m`，三轮通过 **21.655s**，无 race 报告。包含此前长轮询、32 用户隔离、资源裁剪和管理/配对回归。不替代 root 的隔离 Docker / Linux 全包验收。

## 最终门禁：Bridge SSE drain 反例与验证

反例 `TestBridgeStreamAdmissionReplacementCannotExceedActiveBudget/write`：限制为 2 时，同一设备前两条真实 SSE 分别阻塞在 Write/Flush，第三条仍因替换逻辑获准返回 200，先红失败（包耗时 2.395s）。最终修复后第三条返回 429，直到原 handler 真正退出才能重新取得名额。

保持原有“新连接替换旧连接”协议：仍有全局余量时直接替换；满额时不再无条件豁免重连。为避免完全满额时同设备旧连接一直占位，拒绝新请求前仅通知该设备的旧连接关闭；不会借用旧连接名额，也不会关闭其他设备连接。原连接返回后减计数，新请求按现有重试机制再次准入，响应同时带 `Retry-After: 2`。

新增 6 个顶层回归覆盖：

1. Write/Flush 未 drain 时连续替换不能绕过上限；旧 handler defer 不会清掉新 `dev.conn`，重复取消不重复减计数。
2. 管理员断开、原 account 删除重建、手机撤绑再配对，均不提前释放 Bridge 名额；另一 owner 的满额请求不能关闭已准入设备。
3. 完全满额下正常连接能退出并重新连接，防止“限制生效但正常重连死锁”。
4. 请求取消、正常写完成、写 deadline 错误和 Hub shutdown 都覆盖；30 秒写截止保持不变，shutdown 等实际 handler 返回后结束。
5. 65 条已从连接表移除但未 drain 的合成请求，从 performance 降为 economy 仍计入预算；退到 63 才可新增第 64 条，升档后可继续进入，最终计数归零。
6. 同一设备 32 次并发替换，在合成上限 8 下只准入 8、拒绝 24；取消/截止后总计数归零。手机长轮询使用独立预算，不被 Bridge 满额误拒绝。

最终相同源码的扩大定向命令：

```text
go test ./internal/hub -run '^TestBridgeStreamAdmission|^TestSSEAdmission|^TestLongPollAdmission|^TestAppEventsLongPoll|^TestConcurrentUsers|^TestOneUsersFullCommandQueue|^TestLiveResourceTrim|^TestGlobalCacheBudget|^TestModeChangeDoesNotCancel|^TestAdmin|^TestQRSingleUse|^Test.*Bridge|^Test.*Standby' -count=1 -timeout=2m
```

普通通过 **4.780s**；同集合加 `-race -count=3 -timeout=3m`，三轮通过 **24.813s**，无 race 报告；`git diff --check` 通过。包含上述所有准入回归以及既有 Bridge、Standby、管理、配对、多用户与缓存测试。不替代 root 的隔离 Docker / Linux 全量验收。

## 官方入口与保留边界

- Dockerfile 构建 `cmd/hub`，该入口调用 standalone。`loadResourceMode` 对空配置使用 `economy`，持久化模式只接受三种有效模式，因此正式发布入口具有全局预算。
- 内嵌/旧兼容调用 `hub.New(Config{})` 的空模式仍保留原来的无全局上限行为。本轮不改变该兼容默认；App 仍有每 owner 上限，不能把它描述成具有正式 standalone 的全局容量保证。
- 全站 event cache、持久化回执和命令并发仍为共享预算。全局缓存压力可能使另一用户重新从电脑读取近期历史；这不是串号，也不是每用户独立资源保证。
- 既有 32 用户测试验证相同 UUID 分 owner 去重、伪造回执拒绝、事件隔离和单人撤绑；本轮 96 次合成读取验证准入计数，二者都不是承诺 32/96 人长期稳定容量。
- 新测试使用内存 HTTP recorder、可控阻塞 writer 和合成配对，无真实服务器、模型、账号数据或付费调用。阻塞 writer 已确定性覆盖 Write/Flush 未 drain 的准入漏洞；真实 TCP/TLS 慢消费者、长时间峰值、弱网、目标服务器 RSS/CPU 和磁盘压力仍需部署环境验收。
- 手机对 429 的退避及连接状态处理由根任务单独负责，本轮未修改手机代码。

## 根任务冻结后 Docker / Linux 验收（2026-10-14）

App SSE、空长轮询、Bridge SSE 三阶段生产修复全部冻结后，根任务在本机 Docker Desktop Linux 构建 `salcara-hub:release-audit-20261014-local`（amd64，测试版本 `0.5.2-audit-20261014`）。Dockerfile 内实际执行：

- `go test ./...`：hub **12.801s**、launcher **7.860s**、standalone **29.928s**，通过。
- `go vet ./...`：通过。
- `CGO_ENABLED=1 go test -race ./...`：hub **32.905s**、launcher **18.382s**、standalone **293.242s**，通过，无 race 报告。
- Hub 与 launcher Linux amd64 编译、scratch/nonroot 镜像构建完成。镜像 index digest：`sha256:c99c06b9415b6448c7cbe783596c39ba835fd94a58842de98283b3b01016bbc8`。此为本地审核镜像，不是正式发布产物。

随后对该镜像运行 `node deploy/standalone/container-smoke.mjs salcara-hub:release-audit-20261014-local`，实际通过首次初始化拒绝覆盖、管理密钥登录、Secure/HttpOnly/SameSite cookie、CSRF、重启保留设置、改密钥废除旧密钥/会话、运行中拒绝离线重置、停机恢复、恢复后旧密钥失效、日志不含合成秘密，以及 nonroot/只读文件系统/资源限制/healthcheck/更新状态。

烟测兼容性修复：Windows 将复制到宿主的文件 mode 表现为 `0666`，不能用它判断 Linux 容器原文件权限。首次运行在离线恢复文件的旧断言失败（438 != 384，即 0666 != 0600），不是容器原文件放宽权限。两处复制统一使用 `copyPrivateFixture`：所有平台用系统 tar 检查 `docker cp` 归档中的**源文件必须是单个普通 0600 文件**，Linux 继续额外检查落盘 0600。新增 `private-archive.mjs` 与 10 项回归，包括 Windows 系统 tar 实际解析合成 TAR、宽权限/链接/目录/多文件/错误名称/畸形归档拒绝。最终烟测 exit 0，未跳过权限验证。

仅创建明确标记的随机测试容器/卷；finally 校验归属后删除，复查列表为空。只使用新生成的合成密钥，没有挂载 docker.sock、真实数据或账号配置，没有部署服务器或推送镜像。空闲瞬时内存约 7.55 MiB 只是一份启动样本，不能用于承诺生产容量。先前主动中止的两次旧快照构建不计入本节通过结果。

手机配套增量：事件读取 429 保留原在线/错误状态及游标，交给现有指数退避；网络/鉴权错误不被抹去。根任务手机最终 1,958 项全量和 typecheck 通过，详见手机专项报告。真实手机/目标服务器的弱网、休眠和长时容量仍未被上述本地验证替代。
