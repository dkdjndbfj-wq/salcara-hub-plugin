# Salcara Hub

让手机与电脑上的 AI 工作台连接起来。

Salcara Hub 是可自托管的远程编程连接服务，适合个人团队和中转站运营者。它负责设备配对、消息转发和连接管理，需要与 Salcara 手机 App、[桌面 App](https://github.com/dkdjndbfj-wq/salcara-desktop) 配套使用。

手机 App ⇄ Salcara Hub ⇄ 桌面 App ⇄ 电脑上的 Agent

Hub 不运行大模型，也不要求用户提交中转站账号或模型 API Key。任务由电脑端执行；管理人员可以在独立网页控制台管理连接和设备。

## 主要功能

- 手机扫码绑定电脑，绑定后即可使用，无需逐个审批。
- 查看设备连接状态，测量 Hub 与电脑的往返延迟，撤销绑定、断开或封禁设备。
- 提供省资源、均衡、高负载三种运行模式，适配不同服务器条件。
- 单个 Docker 容器部署，提供可视化管理控制台。
- 管理密钥自动生成，可在控制台更换；遗失后可在服务器恢复，保留设备数据。
- 支持签名验证的 Hub 程序更新，与现有中转服务分别维护。

远程会话、消息和 Agent 操作需要电脑在线，并以桌面端已安装、已授权的能力为准；仅部署 Hub 不会自动开放 Agent 未提供的原生桌面接口。

## 快速部署

准备一台 Linux 主机、Docker Engine / Compose、自己控制的域名和 HTTPS 反向代理。支持的镜像平台为 `linux/amd64` 和 `linux/arm64`。

使用正式版本源码，在本地审阅脚本后执行：

```sh
git clone --branch standalone-v0.5.0 --depth 1 https://github.com/dkdjndbfj-wq/salcara-hub-plugin.git salcara-hub
cd salcara-hub
bash deploy/standalone/install.sh \
  --public-url https://relay.example.com/salcara-hub \
  --install-dir /opt/salcara-hub
```

把示例域名换成自己的域名。安装脚本会创建独立 Hub 服务，给出管理地址、密钥文件位置及反代配置片段；它不会安装 Docker、修改现有反代或停止其他服务。已有 Hub 数据的用户请走升级步骤，不要重新初始化。

完整的反代配置、手动安装、迁移和验收步骤见 [部署教程](docs/STANDALONE-DOCKER.zh.md)。也可以把 [Agent 部署指南](docs/AGENT-DEPLOY.zh.md) 交给你的编程 Agent，完成后让它提供实际 URL 和待验收项。

## 开始使用

1. 完成 HTTPS 反代配置，打开 `https://你的域名/salcara-hub/admin/`。
2. 在自己的安全终端或编辑器中读取安装输出指定的初始管理密钥文件，输入整行密钥进入控制台。没有用户名；不要把密钥放进 URL 或发给别人。
3. 在 Salcara 桌面 App 中填写站点根地址，例如 `https://你的域名`，连接后生成配对二维码。
4. 用 Salcara 手机 App 扫码绑定，选择电脑和 Agent，加载会话或发起任务。

模型 API 由桌面 App 配置，不需要填进 Hub 后台。普通用户绑定电脑也不需要管理员密钥。

如需嵌入现有中转站的侧边栏，添加两个同源自定义 iframe 页面：

| 页面 | URL 路径 | 可见范围 |
| --- | --- | --- |
| 管理控制台 | `/salcara-hub/admin/` | 管理员 |
| 连接说明 | `/salcara-hub/` | 普通用户 |

将路径拼在你自己的 HTTPS 站点地址后。管理页面仍需密钥登录；菜单可见范围不能代替权限验证。

## 更新与维护

控制台的「检查更新 / 一键更新」只更新签名验证后的 Hub 程序。Docker 基础镜像和启动器通过 Compose 升级，不会更新或控制你的中转服务。所有升级、恢复和卸载都应先备份，保留原项目与数据卷。

- [部署、升级、恢复密钥与卸载](docs/STANDALONE-DOCKER.zh.md)
- [交给 Agent 部署](docs/AGENT-DEPLOY.zh.md)
- [正式版本下载与说明](https://github.com/dkdjndbfj-wq/salcara-hub-plugin/releases)
- [发布维护说明](docs/STANDALONE-RELEASE.zh.md)

## 安全与资源

默认只向宿主机回环地址发布端口，由 HTTPS 反代对外提供服务；不挂载 Docker socket。默认容器内存上限为 384 MiB，这是资源配额，不是实际占用或承载用户数保证。

聊天历史主要保存在电脑端；Hub 的有限暂存不是永久云端备份。当前不是端到端加密，服务运行方能够读取转发内容。请使用你信任的服务地址，并保护管理员密钥、设备凭据及数据备份。

本项目开放源码，许可证及第三方归属见 [LICENSE](LICENSE) 和 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
