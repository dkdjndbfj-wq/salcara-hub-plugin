# 下一版本：任务恢复、待发撤回和设备清理

本说明记录 2026-11-12 开发并纳入 Hub 0.5.6 的恢复协议，不改变最低兼容版本；正式资产与部署状态以发布记录为准，见 [0.5.6 版本说明](RELEASE-0.5.6.md)。

## 手机端行为

Hub 宣告 `commands.queue-cancel.v1` 后，手机可对原 queued 任务显示“撤回待发”。停止 HTTP 等待只结束本地等待；停止已运行 Agent 仍走既有停止任务命令。Bridge 不增加撤回消息。

`POST /v1/app/operations/cancel` 仅接受 `{deviceId,operationId,ticket}`，使用原身份凭据和原票据，不续签或分配新编号。撤回与派发竞争同一 queued 行；撤回标记和确定未派发回执同事务提交。数据库提交失败不显示成功，未确认时查询原操作或重试原撤回。

| 情况 | HTTP | code / 状态 |
| --- | --- | --- |
| queued 撤回成功、已撤回重试 | 200 | `operation_cancelled`，`ok:true,state:cancelled,cancelled:true` |
| 原待发期限已过 | 409 | `offline_dispatch_expired`，`state:expired` |
| 已开始派发或已结束 | 409 | `operation_not_cancellable` |
| 没有 queued 记录 | 409 | `operation_not_queued` |
| 撤回保存失败 | 503 | `operation_cancel_unavailable,retryable:true`，仅重试原撤回 |

取消成功响应带原 `operationId,storageEpoch,dispatchNotAfter`。GET 原操作返回 HTTP 200、`state:cancelled,ok:false,code:operation_cancelled,cancelled:true,httpStatus:409`，并带原期限和 `retainedUntil`；这里 `ok:false` 表示业务任务未执行。正文清理、重启和票据过期不会改变撤回事实。原 commands/receiptOnly 重试返回 409 `operation_cancelled`。

手机在发起撤回前保存 cancelRequested 和原映射；网络失败后先恢复原状态。GET cancelled 确认成功，queued 可重试撤回，pending/uncertain 等不能自动创建业务任务。新手机、其他设备/组或已撤销凭据不能操作旧代次。

## 恢复语义

- 需要升级的桌面明确返回 426 `update_required`，不会误标为结果未知。
- uncertain 和结果截断事实独立于正文保留期；清理不把未知执行结果改成“从未执行”。
- 调度每轮检查有界队列，跳过仍离线电脑后继续寻找在线电脑；最多派发八项。
- 入队前检查容量；队列满返回 429 `offline_queue_capacity`，原准备身份不变，未消耗派发回执。
- 长期停机后即使回执文件已到期，尚未 claim 的过期队列项也会终结并释放正文。损坏的未派发项不会自动补发，GET `not_dispatched` 提供 `offline_queue_unavailable`。确认入队保存失败为 `offline_queue_registration_failed`。

## 管理与清理

系统页显示全站及单连接最高任务身份/安全回执、消息接收身份的用量和上限。达到约 80% 提示，读取失败明确显示，满额拒绝新身份。清理到期内容只释放正文空间，不删除防重复执行身份；不建议通过删除数据库恢复容量。

Bot 手机绑定使用“已配对”清理策略，已撤销邀请不等于手机解绑。待确认登记、手机变更和封禁记录保留。只对规范设备账号同步组成员，旧 API 账号的同名设备不改变 Bot 权限。

删除先原子保存设备表，再移除内存设备/组成员并保存 Bot 状态。首写失败不删除；第二次写失败不回滚已提交删除，管理页显示同步未完成，可在修复存储后重试。启动在服务开放前删除孤儿成员并保存；修复无法持久化时拒绝启动。普通站点清理和关闭不写 Bot 数据。

本地自动化使用临时存储、合成指令和故障注入，不代表真实手机跨端验收或真实断电试验。发布前需与带 capability 判断的手机实现联合验证。
