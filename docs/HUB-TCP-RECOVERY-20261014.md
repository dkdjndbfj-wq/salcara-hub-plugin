# Hub 真实 TCP 断连与慢消费者恢复验证（2026-10-14）

## 本轮结论与改动

新增 `internal/hub/tcp_recovery_test.go`，补齐此前 `REMOTE-READ-ADMISSION-AUDIT-20261014.md` 仅通过合成阻塞 writer 验证的实际 TCP 路径。本轮未发现需要再修改 SSE 生产代码的新缺陷，因此没有修改 `server.go`、资源额度、生产 30 秒写截止或 App / Bridge 协议。

已有活跃 handler 准入修复在本机真实 loopback TCP 上通过：慢消费者队列被移除时仍占名额；原生写 deadline 触发后释放；重连按已接收游标重放；FIN / RST 断连释放 SSE 与长轮询预算；另一用户继续收流。

## 测试如何证明实际发生了阻塞

测试使用 `httptest.NewUnstartedServer` 启动真实 `net/http.Server`，监听本机临时 TCP 端口，客户端直接使用 `net.DialTCP` 写 HTTP 请求和读取 chunked SSE。连接包装层记录每次真实 `net.TCPConn.Write` 的开始、结束和错误；不会合成 Write / Flush 阻塞或超时错误。

初始 fixture 以 256 KiB 事件及 1 KiB socket buffer 推断阻塞，在 Windows loopback 上实际 Write 完成且返回 nil，因此该断言失败。这是测试假设错误，不是新生产漏洞。最终 fixture 持续投入单个 256 KiB 的有效大小帧，直到**实测同一个原生 Write 持续未返回**，才开始验证预算与超时。最多投入 32 MiB，无法造成阻塞时明确失败，不会跳过测试或把成功 Write 当作慢消费者。

这些用于填充 TCP 接收窗口的帧通过现有账号消息分发函数进入有界队列，不写事件缓存，避免测试造压挤掉后续要检查的重放数据。后续事件上传、容量拒绝、重连和重放均经过真实 TCP HTTP 路由。测试账号与初始配对复用已有合成 fixture，无真实用户或模型凭据。

为了避免每次全包测试额外等待 30 秒，测试连接把**已经由生产代码设置**的写 deadline 缩短为 2 秒。测试同时断言 Hub 请求的原始 deadline 仍为 30 秒，并要求最终错误为原生 `net.Error` 且 `Timeout() == true`。没有修改生产超时常量，也没有人为返回超时错误。此项证明 native socket deadline 连通性，不是 30 秒墙钟精度或真实网络的长期性能测量。

## 已覆盖场景

### 慢消费者、容量与游标恢复

`TestTCPRecoverySlowAppTimeoutRetainsBudgetAndReplays`：

1. 两个独立配对 owner 各开一条真实 App SSE；第一用户先实际读到一条基线事件并保存游标。
2. 第一用户停止读取，实测 native Write 阻塞；再上传事件使消息队列溢出并从订阅表移除。
3. 在 handler 尚未退出期间，该 owner 的新 SSE 和空长轮询均收到 HTTP 429、`app_read_capacity`、`retryable: true`、`Retry-After: 2`。
4. 第二用户在第一用户仍阻塞时继续收到自己的事件，验证独立用户不会被该 owner 占位或队列溢出断开。
5. native 写 deadline 自然触发，第一用户活跃计数归零，全局仍保留第二用户。
6. 第一用户使用实际读到的游标重连，按连续序号读回 34 条缓存事件（1 条 256 KiB 事件及 33 条小事件），验证没有重复基线或串入另一用户事件；随后新事件可继续在线投递。
7. 关闭连接后 App / 长轮询计数归零。

### FIN / RST 与三种读取恢复

`TestTCPRecoveryDisconnectReleasesAndReconnects` 的六个子场景为 `app/fin`、`app/reset`、`bridge/fin`、`bridge/reset`、`long-poll/fin`、`long-poll/reset`。

每个场景都以真实 socket 关闭触发服务端请求结束，不主动取消合成 Context。`reset` 使用 `SetLinger(0)` 后关闭；`fin` 使用正常关闭。断连后核实原 owner / 全局预算释放，Bridge 同时核实设备连接已清空；随后同用户重新准入并读取初始 SSE 状态，或以新事件唤醒重新进入的长轮询。所有场景同时保留另一用户的 SSE，并检查其仍能接收自己的事件；最终所有真实 handler 计数归零。

测试将局部额度缩为每 owner 1、App 全局 2 或 3、Bridge 全局 1，以便直接观察释放后才能重连。这不改变默认三种资源模式的额度，也不构成生产容量基准。

## 本机验证

使用既有 Go 1.27.1 Windows 工具链：

```text
go test ./internal/hub -run '^TestTCPRecovery' -count=1 -timeout=45s -v
```

通过，包耗时 **2.485s**；两项顶层测试、七项实际场景全部通过。

使用已有 `portable-race-toolchain/extracted/w64devkit/bin` 和 `CGO_ENABLED=1`：

```text
go test ./internal/hub -run '^TestTCPRecovery' -race -count=3 -timeout=1m
```

三轮通过，包耗时 **8.270s**，无 race 报告。最初环境默认 `CGO_ENABLED=0` 的 race 命令被 Go 拒绝；启用已有本地工具链后的上述结果才是有效 race 验收。`gofmt` 与 `git diff --check` 通过。

根任务后续对冻结源码执行 Linux / Docker 全包测试、race 和容器烟测；本报告不提前声明这些尚未由本轮完成的结果。

## 保留边界

- 本轮新增的是 HTTP/1.1 loopback TCP 验证，未测试 TLS、HTTP/2、反向代理缓冲或目标服务器的真实公网弱网。
- TCP 慢写及队列溢出的实网路径在 App SSE 上覆盖；Bridge SSE 新增 FIN / RST 和重连验证，Bridge 慢写准入仍另有既有合成 writer 回归，不应把本轮说成所有传输的真实慢写压测。
- 未做手机后台休眠、网络切换、长时间峰值、生产磁盘或 RSS / CPU 容量验收；没有发布、部署、推送镜像或调用真实模型。
- 本轮只新增测试和本专项报告，保留所有既有未提交修改；此前审计文档中的已知剩余边界，以本报告完成的具体 TCP 场景为增量更新。
