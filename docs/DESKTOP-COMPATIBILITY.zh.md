# 桌面版本兼容性

默认不限制桌面版本。协议变更需要阻止旧桌面发起新任务时，可在 Hub 环境中设置：

```dotenv
SALCARA_HUB_DESKTOP_MINIMUM_VERSION=1.6.5
SALCARA_HUB_DESKTOP_UPDATE_URL=https://github.com/dkdjndbfj-wq/salcara-desktop/releases/latest
```

最低版本必须为三段数字，下载地址必须为 HTTPS；无效配置会阻止 Hub 启动。两个 Compose 文件均透传这两个可选变量。修改后需重新创建或重启 Hub 服务，使配置生效。

设置最低版本后，旧版或未提供版本的电脑发起新任务会收到 HTTP 426 和 `update_required`。检查同时位于请求入口和最终派发处，避免排队期间连接版本变化绕过限制。已有任务的事件、回复、审批、停止和历史查询继续可用。`ping` 响应的 `desktopCompatibility` 给出最低版本及下载地址。

先发布并验证可安装的新桌面版本，再启用版本门槛。1.6.4 及更早的客户端没有新版强制更新界面，应先通过普通更新或安装包迁移到 1.6.5；迁移期间保持旧协议和下载入口可用。设为空值并重启可取消 Hub 的版本门槛，但不会取消桌面端已经验证并缓存的签名更新策略。桌面策略的调整需要发布新的签名版本清单。
