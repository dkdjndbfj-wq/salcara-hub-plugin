# 文档目录

## 部署与使用

- [项目首页](../README.md)：独立 Docker Hub 介绍。
- [Docker 部署教程](STANDALONE-DOCKER.zh.md)：0.5.3 安装目标、HTTPS、iframe、管理员账号与密码、更新、恢复和卸载；执行前确认对应 Release 和镜像已发布。
- [交给 Agent 部署](AGENT-DEPLOY.zh.md)：部署边界和交付验收清单。
- [服务端入口与线路管理](SERVER-NETWORK.zh.md)：HTTP/SOCKS5/Mihomo 接入、订阅兼容性检测、节点池与真实加速边界。
- [代理池调度、检测与用户状态页](PROXY-SCHEDULING-AND-STATUS.zh.md)：地区规则、自动选路、检测默认值与范围、故障恢复和公开状态页。
- [托管订阅与代理池使用说明](MANAGED-PROXY-POOL.zh.md)：管理员接入、兼容矩阵、自动更新、故障恢复和本地验证边界。
- [配置安全发布与运行观测](EDGE-SAFE-RELEASE-AND-OBSERVABILITY.zh.md)：预检、试运行、回滚 CLI、检测取消、共享预算和趋势 API。
- [同一个 Hub 的多个 HTTPS 入口](MULTI-INGRESS-HA.zh.md)：电脑与手机透明切换、签名身份、缓存时限及入口管理。
- [节点网络归属观察](NODE-NETWORK-OBSERVATION.zh.md)：本地国家与 ASN 数据、自动观察、地区策略和失败恢复。
- [订阅来源共享并发额度](SUBSCRIPTION-SHARED-CAPACITY.zh.md)：全池、来源、节点三层准入与跨代连接计数。
- [全站远程使用授权](REMOTE-SOURCE-AUTHORIZATION.zh.md)：本站／外站 API、反代账号独立开关、地址名单、签名状态和旧客户端升级。
- [管理员登录、用户与设备管理](USER-MANAGEMENT.zh.md)：用户档案、登记码归属、禁用／启用、单独来源名单、设备筛选与配对管理。
- [服务端线路工具产品化路线](NETWORK-PRODUCT-ROADMAP.zh.md)：接入与恢复、托管 Mihomo、订阅自动维护及客户端诊断的设计记录；当前交付范围见下方 GitHub 发布说明。

## 发布与维护

- [GitHub 发布与可用性说明](GITHUB-RELEASE-READINESS-20261108.zh.md)：当前交付范围、使用流程、版本搭配、测试包和未实测项目。
- [商业发布就绪核对（历史参考）](COMMERCIAL-RELEASE-READINESS-20261108.zh.md)：已修复的兼容问题及未来商业部署可参考的检查项，不作为当前 GitHub 发布门槛。
- [构建、签名与更新维护](STANDALONE-RELEASE.zh.md)。
- [0.5.3 更新说明](RELEASE-0.5.3.md)：本轮目标版本、配套客户端与验证范围。
- [0.5.2 更新说明（历史版本）](RELEASE-0.5.2.md)。
- [0.5.1 发布验收](RELEASE-0.5.1-VERIFICATION.md)。
- [CI 开发产物说明](../deploy/standalone/CI-ARTIFACT.md)：不是签名更新包。
- [项目许可证](../LICENSE) 与 [第三方声明](../THIRD_PARTY_NOTICES.md)。

独立部署默认使用 `open`（仍需设备配对），执行范围始终受 `station` 限制。
未配置 `station` 时，只从可信的 `SALCARA_HUB_PUBLIC_URL` 去掉完整 `/salcara-hub`
后缀推导本站 API 根地址；没有可信公开地址时范围为空，远程执行拒绝，不使用请求 Host 推导权限。
需要部署远程来源策略时设置
`SALCARA_HUB_REMOTE_ACCESS_POLICY_FILE=/绝对路径/policy.json`；策略文件中的
`mode` 可为 `open`、`restricted` 或 `disabled`。`restricted` 要求可信来源签名，
仅允许官方账号时可不配置外部发行方；策略公钥只放在部署配置中，私钥不能进入仓库、镜像或手机。
若同时设置 `SALCARA_HUB_REMOTE_ACCESS_MODE` 与策略文件，环境变量优先决定模式，策略文件提供发行方公钥、受信来源类别、`audience` 和版本。
未设置环境变量时，管理员可在“访问策略”管理页分别设置本站 API、其他中转站 API、反代账号的开关和地址名单，
另行设置官方账号开关。保存使用 StationPolicy v2，并与策略版本原子写入数据卷中的 `remote-access-mode`，不改写发行方公钥。
修改站点范围必须提交当前 `expectedRevision`，过期页面返回冲突。任何策略内容变化都会更新版本，
已有授权不能跨版本使用。部署策略或公开地址变化后，旧的管理页站点范围覆盖失效。
设置环境变量后管理页只能查看，由部署文件配置站点范围。

v2 的 `station.apiBases`、`externalApiBases`、`reverseProxyApiBases` 每份最多 32 项，合计最多 32 个不同地址，按规范化后的完整地址精确匹配：主机名大小写、默认端口、
末尾 `/` 或 `/v1` 等价；保留完整路径，不允许模糊编码、点路径、查询参数或片段。
例如 `https://api.example.com/team-a` 不允许 `/team-b`，同域也不能越过路径范围。
空数组表示关闭该类来源。`allowOfficialAccounts` 默认关闭，独立控制电脑本地保管的官方账号。
发行方授权只能进一步收窄本站范围，不能放宽它。

远程模型查询也受上述范围约束。v2 执行需要电脑声明 `station.guard.v2`；旧 Bridge 需升级。历史 v1 策略仍兼容，其普通 API 与反代共用原名单；管理台保存时保留范围并迁移到 v2。
站点切换仅传输连接控制，目标 Hub 验证及重新绑定由电脑完成；切换后执行使用目标站点策略。

最小策略文件示例（公钥为不带 `=` 的 base64url Ed25519 公钥）：

```json
{"mode":"restricted","revision":"r1","audience":"my-hub","station":{"version":2,"apiBases":["https://api.example.com/team-a"],"externalApiBases":[],"reverseProxyApiBases":[],"allowOfficialAccounts":false},"issuers":[{"id":"s1","publicKey":"<ed25519-public-key>","sourceTypes":["site_api"],"agentIds":["codex","claude","hermes"],"origins":["https://api.example.com"],"apiBases":["https://api.example.com/team-a"],"authorizationUrl":"https://api.example.com/source-grants"}]}
```

只允许官方账号时可使用：

```json
{"mode":"restricted","station":{"version":2,"apiBases":[],"externalApiBases":[],"reverseProxyApiBases":[],"allowOfficialAccounts":true}}
```

此时受众默认 `station`。电脑通过已认证的挑战回复证明本地官方账号，Hub 使用进程内临时
Ed25519 密钥签发短期来源授权，保留发行方标识 `station-official`；外部发行方不能占用此标识。
Hub 不接收提供商 token，服务重启后旧的官方授权失效，手机重新取得授权。

## 架构评审

- [远程消息持久存储与自动清理设计](DURABLE-REMOTE-MAILBOX-DESIGN-20261107.zh.md)：保留原始设计基线；本地实现状态、接口与验证边界见 [实施说明](DURABLE-REMOTE-MAILBOX-IMPLEMENTATION-20261107.zh.md)，尚未部署。
- [来源发行协议](SOURCE-ISSUER-CONTRACT.md) · [来源发行服务](SOURCE-ISSUER-SERVICE-20261015.md)
- [远程只读准入审计](REMOTE-READ-ADMISSION-AUDIT-20261014.md)
- [后续架构维护](HUB-CONTINUATION-20261014.md) · [TCP 恢复](HUB-TCP-RECOVERY-20261014.md) · [更新恢复](HUB-UPDATE-RECOVERY-20261014.md)
- [发布契约审计](RELEASE-CONTRACT-AUDIT-20261014.md)
- [远程 API 来源与访问策略设计](REMOTE-ACCESS-POLICY-DESIGN-20261009.md)：运行时已提供 `open/restricted/disabled` 三种执行策略；`restricted` 通过签名 grant 区分本站 API、可信反代和未来 GPT 账号反代，覆盖 A→B 站切换的授权边界。

用户不需要安装 Sub2API 插件或替换中转站宿主。GitHub 仓库名保留历史兼容标识，当前产品是独立 Hub；手机和桌面 App 分别维护。

## 新增运维能力

- [协作管理、审计、监控与备份恢复](OPERATIONS-AND-RECOVERY.zh.md)：当前开发源码的角色权限、故障判断、外部告警和完整数据卷恢复流程。
- [三端产品收尾记录](PRODUCT-COMPLETION-20261108.zh.md)：本轮源码实现、本机验证与尚需真实发布环境的交付边界。
