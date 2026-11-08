# Edge 配置安全发布、恢复与线路观测

所有接口均位于 Edge 的独立管理监听器，要求 `Authorization: Bearer <独立控制令牌>`。不要公开此端口，也不要把令牌放进 URL。下述接口不接收 URL 查询参数；查询筛选放在 JSON 请求体内。手机客户端和桌面客户端无需配置机场凭据或选择节点。

## 节点与策略的安全发布

管理界面应先准备候选配置，等待实际 Hub 连通性验证，再启动试运行。准备阶段不修改当前节点、路由或节点健康状态。

```http
POST /v1/releases/prepare
Content-Type: application/json

{
  "expected_revision": 12,
  "action": "policy",
  "payload": {
    "mode": "latency"
  }
}
```

`action` 支持 `nodes`、`nodes/delete`、`policy`。`payload` 使用对应原接口的请求字段，但不包含 `expected_revision`。例如节点编辑使用 `{"node":{"id":"…","name":"…","enabled":true}}`；删除使用 `{"id":"…"}`。节点编辑省略 `proxy_url` 可保留已有凭据，显式修改来源节点的代理地址会解除它与订阅来源的归属关系。

准备返回 `202` 和完整 `State`。随后轮询 `GET /v1/state` 的 `release.previews`：

| 字段 | 含义 |
| --- | --- |
| `id` | 本次候选编号 |
| `status` | `checking`、`ready` 或 `rejected` |
| `base_revision` | 候选基于的当前配置版本 |
| `created_at`、`expires_at` | Unix 毫秒时间 |
| `total`、`completed`、`healthy` | 启用节点总数、已完成数、通过数 |
| `summary` | 操作类型 |
| `reason` | 脱敏机器码，如 `changed_node_unreachable`、`no_verified_route` |

检查固定的 `/salcara-hub/statusz`，只接受不超过 4096 字节、HTTP 200 且 `status=available` 的响应，不跟随重定向。整个候选检查最多 60 秒，单节点最多 6 秒，新增、重新启用或连接参数变化的启用节点必须通过检查；候选策略也必须有至少一条经过验证的可用路径。最多保留 4 个候选。验证完成后的 `ready` 候选有效期为 30 秒，过期需重新检查。

```json
POST /v1/releases/start
{"expected_revision":12,"preview_id":"候选编号"}
```

启动成功返回 `200` 和 `State`，增加配置版本并创建固定 **120 秒** 的试运行：

```json
{
  "release": {
    "trial": {
      "id": "试运行编号",
      "base_revision": 12,
      "revision": 13,
      "started_at": 1790000000000,
      "expires_at": 1790000120000
    }
  }
}
```

确认和回滚均使用当前版本和试运行编号：

```json
POST /v1/releases/confirm
{"expected_revision":13,"trial_id":"试运行编号"}

POST /v1/releases/rollback
{"expected_revision":13,"trial_id":"试运行编号"}
```

确认必须发生在到期前。未确认到期会自动回滚；服务重启也会保守恢复未确认配置。待恢复信息与配置一起原子写入私有 `edge-pool.json`，关闭服务不会丢弃这项责任。运行中的回滚若写盘或依赖的托管内核恢复失败，会保留待恢复状态并重试，`release.last_error` 显示 `rollback_pending`。

试运行期间，普通配置修改和订阅发布返回 `409 trial_in_progress`。提交、确认、回滚都校验配置版本，旧操作不会覆盖后来的修改。被恢复配置中的直连节点，只有在当前配置中同一节点仍明确启用直连时才会重新启用；否则保持禁止并显示 `rollback_preserved_direct_block`。这可能暂时没有可用线路，但不会擅自打开直连。

`release.history` 最多保留 8 个私有配置快照的公开摘要：`id`、`revision`、`created_at`、`reason`、`revertible`。公开状态不含快照或凭据。按历史恢复：

```json
POST /v1/releases/rollback
{"expected_revision":15,"version_id":"历史版本编号"}
```

`trial_id` 与 `version_id` 必须只提供一个。历史恢复先准备必要的托管内核进程，依赖无法恢复时返回 `409 rollback_dependency_unavailable`，不会仅凭写回旧地址就宣布恢复成功。已失效的机场账号和外部节点不因历史恢复重新有效。启动阶段的内核恢复问题也会显示在来源状态的 `kernel.error`。

## 独立本机恢复命令

业务入口中断时，可通过 SSH 登录部署服务器，直接调用独立的本机管理通道：

```powershell
salcara-edge recover state --control-listen 127.0.0.1:8791 --control-token-file /path/to/control-token

salcara-edge recover rollback --control-listen 127.0.0.1:8791 --control-token-file /path/to/control-token --expected-revision 13 --trial-id TRIAL_ID

salcara-edge recover rollback --control-listen 127.0.0.1:8791 --control-token-file /path/to/control-token --expected-revision 15 --version-id VERSION_ID
```

命令只接受数字回环地址和数字端口，不使用环境 HTTP 代理，不进行 DNS 解析，不跟随重定向。控制令牌必须来自独立的私有普通文件，内容为 64 个十六进制字符。输出仅包含 `revision`、`status`、`release`，不打印令牌或节点凭据。恢复命令要求 Edge 管理进程仍在运行；它不会绕过认证或离线强写配置文件。

## 可取消的全节点检测

```json
POST /v1/probe
{"scope":"all"}

POST /v1/probe/cancel
{"run_id":"当前 probe_run.id"}
```

`scope=all` 包含停用节点，仅诊断，不会启用它们。`enabled` 只检查启用节点；可选 `node_ids` 限定节点。取消返回 `202` 和完整 `State`：

- `probe_run.cancelled` 立即为 `true`；`running` 在在途检查退出后变为 `false`。
- 已完成的结果保持不变；取消的在途检查和未开始检查计入 `skipped`，不会增加 `failed` 或污染节点健康。
- 完成时 `completed=total`。取消当前已完成任务是幂等操作。
- 不匹配当前任务的编号返回 `409 probe_run_not_found`；旧编号不会取消新的任务。
- 自动检测调度与单独排队的后续任务继续工作。新的任务使用新编号，`cancelled=false`。

普通检测、配置预检、来源候选验证共用 **最多 4 个同时执行的固定 Hub 探测**。各任务还受自身并发限制。等待这个共享额度不是节点故障，可以被对应任务的上下文取消。

## 24 小时趋势

Edge 每分钟采样，保留最近 24 小时、最多 1440 个时间桶。每个桶最多 32 个节点；独立私有文件 `edge-observability.json` 上限为 24 MiB。写盘与路由互斥锁分离，读取或保存遥测失败仅显示错误，不阻止业务路由。正常关闭会保存最后一个未满分钟的桶。

```http
GET /v1/observability
```

默认返回池级汇总和当前选路原因。要查询一个节点及较短时间窗口：

```json
POST /v1/observability/query
{"node_id":"节点编号","window_ms":3600000}
```

`node_id` 可省略；`window_ms` 省略或为 `0` 表示 24 小时，其余允许范围为 60000 至 86400000。默认汇总的每条 `samples[].nodes` 为空，指定节点后每条最多一个节点，避免一次返回整个节点矩阵。

响应字段：

| 字段 | 含义 |
| --- | --- |
| `sample_interval_ms` | `60000` |
| `retention_ms` | `86400000` |
| `counter_semantics` | `bucket_deltas_and_instantaneous_gauges` |
| `samples` | 时间桶序列 |
| `decision` | 当前选路解释 |
| `last_error` | 空字符串或遥测错误机器码 |

每个样本包含 `at`（分钟桶起点，Unix 毫秒）、`revision`、`active_id`、`mode`、`runtime`、`nodes`。节点样本包含 `id`、`status`、`latency_ewma_ms`、`stale`、`runtime`。没有代理 URL、凭据、请求内容或客户端身份。

趋势 `runtime` 的 `completed_requests`、`failed_requests`、`rejected_requests`、`cancelled_requests` 是该时间桶的增量。`active_requests`、`active_streams`、`queued_requests`、`queued_streams` 是采样时的瞬时值。它们与 `GET /v1/state` 中“本进程启动后的累计计数”含义不同。重启、同编号节点重建或计数器回退不会产生负值或无符号下溢尖峰；同一分钟内的多个采样合并计数。

`latency_ewma_ms` 是当前平滑延迟样本；`null` 表示没有有效测量，`stale=true` 表示样本已经过期，图表不应把它当作刚完成的测速。

## 选路原因

`decision` 包含 `mode`、`active_id`、稳定的 `reason`、可读的 `explanation`、池级 `request_capacity` / `stream_capacity` 和 `candidates`。

| 池级原因码 | 含义 |
| --- | --- |
| `manual_selection` / `manual_unavailable` | 指定节点可用 / 指定节点无法作为入口 |
| `weighted_capacity` | 按节点权重与当前占用分配请求 |
| `failover_precedence` | 按指定节点与配置顺序故障切换 |
| `lowest_latency` | 根据平滑延迟选择 |
| `switch_cooldown` | 延迟改善达标，但仍在冷却期 |
| `improvement_below_threshold` | 延迟改善未同时达到绝对值和百分比门槛 |
| `no_eligible_route` | 没有符合策略的节点 |

每个候选包含 `id`、`eligible`、`reason`、`explanation`、`region`、`latency_ewma_ms`、`can_request`、`can_stream`、`request_utilization`、`stream_utilization`。候选原因码为 `disabled`、`draining`、`manual_unselected`、`unhealthy`、`stale_disallowed`、`unknown_disallowed`、`region_excluded`、`capacity_full`、`limited_trial`、`selected` 或 `eligible`。`eligible` 描述策略资格，容量字段分别描述当前能否接纳普通请求和长连接；有资格但容量满时仍可能需要排队。
