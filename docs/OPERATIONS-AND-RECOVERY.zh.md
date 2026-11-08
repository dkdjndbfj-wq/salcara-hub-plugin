# 协作管理、审计、监控与备份恢复

这些能力属于当前开发源码，不能由旧正式版本的版本号推断已经提供。请先使用相同源码构建并完成维护窗口验收。

## 管理角色

原有管理密钥继续作为主管理员，无需迁移。登录后进入「运维中心 → 协作与监控」，按姓名或用途创建独立密钥：

| 角色 | 允许的操作 |
| --- | --- |
| 主管理员 | 全部管理功能；创建、撤销协作与监控密钥 |
| 运维 | 查看设备与聚合状态、审计；测量、断开、封禁、解封、撤销设备配对 |
| 只读 | 查看设备、聚合状态和审计，无写权限 |
| 监控接口 | 仅访问聚合健康和 Prometheus 指标，不可登录管理界面 |

服务器凭据、代理池、访问策略、存储政策、更新、登记码和账号管理仅对主管理员开放。后端按明确的接口白名单授权，新接口默认仅主管理员可用；隐藏按钮不作为权限边界。

最多 32 个独立密钥。密钥使用 256 位随机数，只显示一次，服务器只持久保存摘要。撤销后已有会话及后续监控请求失效；已获准开始的操作可能完成，撤销不回滚这些操作。密码/密钥、Cookie 和 CSRF 值不进入审计正文。主密钥更换会退出全部浏览器会话，协作密钥仍有效；人员离开或密钥泄露时需要单独撤销。离线 `-reset-admin-key` 同样只恢复主管理员入口。

## 审计与健康

「操作记录」新增可查询和导出的管理审计，按操作者名称、操作路径和结果筛选。查询和导出覆盖最近 30 天，文件最多 5,000 条；过期条目在下一次管理写入时清理。导出为 NDJSON。原有设备操作记录保留，用于查看具体原因和设备标识。管理审计记录设备操作枚举及匿名设备标识，不保存操作正文、订阅链接或聊天内容。

每个已授权的管理写请求先保存未确认记录，再执行。不能持久保存时返回 503，操作不开始。执行后补写 HTTP 结果；中断或结果写盘失败时保留未确认状态，先核对实际状态再决定后续动作。审计不是防本机管理员篡改的不可变账本，也不是永久归档；需要长期记录时定期导出到独立存储。

运维中心可见且浏览器在前台时每 30 秒读取健康状态。离开页面或关闭会话停止自动请求。状态包含磁盘预留不足、存储容量告警、审计写入异常、备份逾期和推送是否配置。页面检查不能代替外部告警：服务停机时它无法自行发送通知。

外部采集使用独立监控密钥，放入采集器的私有文件，不能拼在 URL 中：

```yaml
scrape_configs:
  - job_name: salcara-hub
    scrape_interval: 60s
    metrics_path: /salcara-hub/_monitor/v1/metrics
    scheme: https
    authorization:
      type: Bearer
      credentials_file: /run/secrets/salcara-monitor-key
    static_configs:
      - targets: [hub.example.com]
rule_files:
  - /etc/prometheus/salcara-monitoring-rules.yml
```

将 `deploy/standalone/monitoring-rules.yml` 加入 Prometheus，再通过自己的 Alertmanager 配置接收渠道。`/_monitor/v1/health` 返回同一组聚合状态；请求成功返回 200，需要读取 `status` 和 `issues` 判断健康程度。接口不暴露设备名称、设备凭据、会话正文或订阅参数。未配置采集器与接收渠道时，不应宣称外部告警已经启用。

## 离线加密备份

备份工具覆盖整个 Hub 数据目录，包括设备配对、管理凭据、消息数据库及 WAL、图片分片、加密密钥、入口身份、策略和已下载的 Hub 更新程序。归档使用带逐块认证和结束标记的 AES-256-GCM，并含每文件 SHA-256、大小和受限权限清单。创建后会完整读回校验，再记录备份时间。

单个归档最多 128 GiB 的文件内容、100,000 个文件。拒绝符号链接及特殊文件。恢复文件仅保留拥有者读写权限，原有拥有者执行位保留，不恢复组/其他人或特殊权限。备份密钥单独保存到数据卷外的 32 字节私有文件；工具首次备份时可生成该文件，不会打印密钥。遗失密钥无法解密归档。

备份期间必须停止 Hub 和启动器。同一卷的独占锁会阻止对正在使用的 Hub 备份。不要删除锁文件。维护期间用户连接会中断，电脑上的任务可能仍在继续。

本机二进制使用现有部署环境变量：

```sh
# 先停止管理该 Hub 的服务；保留原目录。示例目录需预先创建并仅供服务用户访问。
salcara-hub -backup /backup/hub.scbackup -backup-key-file /keys/hub-backup.key
salcara-hub -verify-backup /backup/hub.scbackup -backup-key-file /keys/hub-backup.key
```

Docker 安装时，从原 Compose 目录创建一次性维护容器。以下目录需要提前以 UID/GID 65532 配好读写权限，替换为自己的路径：

```sh
docker compose stop hub
docker compose run --rm --no-deps --entrypoint /salcara-hub \
  -v /srv/salcara-backups:/backup -v /srv/salcara-backup-keys:/keys \
  hub -backup /backup/hub.scbackup -backup-key-file /keys/hub-backup.key
docker compose run --rm --no-deps --entrypoint /salcara-hub \
  -v /srv/salcara-backups:/backup:ro -v /srv/salcara-backup-keys:/keys:ro \
  hub -verify-backup /backup/hub.scbackup -backup-key-file /keys/hub-backup.key
docker compose start hub
```

如使用 `compose.release.yml` 或安装脚本生成的文件，在每条命令中使用原来的 `-f`、项目名和 env 文件。不可误用另一个 Compose 项目。备份成功后将归档复制到独立存储，密钥分开保存。归档内凭据虽已加密，仍需限制分发。

备份**不包含数据目录外**的 Compose/env 文件、反向代理和 TLS 配置、FCM 服务账号、独立 Edge/Mihomo 数据或服务器凭据文件。它们应使用各自的备份方案，与 Hub 归档一同列入恢复清单。

## 恢复演练

1. 保留旧卷和旧程序，准备一个不存在的新目录。工具拒绝任何已存在目录，即使目录为空。
2. 使用备份对应版本的程序和正确密钥，执行恢复；先写入同父目录下的私有暂存目录。
3. 工具校验逐块认证、归档结束、每文件清单、SQLite 完整性、数据库解密哨兵和管理凭据，全部通过后才发布新目录。Linux 使用不替换目标的原子重命名；Windows 也拒绝替换现有目录。
4. 配回原部署模式、公开 URL 及卷外依赖，以同版本启动新目录；先验证配对、读取旧结果、图片、权限和入口身份，再切换数据卷。

```sh
SALCARA_HUB_DATA_DIR=/restore/hub-new \
SALCARA_HUB_ADMIN_ACCOUNT_FILE=/restore/hub-new/admin-account.json \
salcara-hub -restore-backup /backup/hub.scbackup -backup-key-file /keys/hub-backup.key
```

Docker 可继续使用一次性维护容器，将准备好的父目录映射为 `/restore`，并传入上面两个环境变量。`/restore/hub-new` 必须尚不存在；父目录需可写。原 `/data` 卷保持不变，恢复不会自动切换生产挂载。

恢复成功只证明数据通过本地验证，不代表客户端、反代和推送都已经验收。不要同时启动旧卷与克隆卷为同一个站点服务。恢复较旧备份可能重新带回当时尚未撤销的设备或管理凭据，切换前需核对并重新撤销。程序回滚和数据恢复是两个不同操作。
