# 给 Agent 的独立 Hub 部署指南

只部署 Salcara Hub，不修改桌面/手机 UI，不操作用户模型 Key，不把 CLI 验收说成原生桌面能力验收。Bot 是电脑与手机之间的中继，Codex、Claude CLI 和其他 Agent 仍运行在用户电脑。遵循用户授权；不得把服务器密码、管理密钥、设备秘密、原站 JWT、私钥或真实配置提交 GitHub、写入公开报告或发到聊天。Bot 模式验收成功后，将受限绑定 JSON 交付给用户；该包最多可登记 8 台电脑和 1 部手机。绝不输出管理密钥。

## 部署前只读检查

1. 确认 SSH 主机身份、系统/架构、内存和磁盘余量；密码仅通过不回显登录输入，不写命令参数或文件。
2. 检查本机 Docker/Compose、容器名/镜像/端口、Hub 数据卷和目标安装目录。不要全量输出容器 Env、`.env`、数据库密码或日志。
3. 确认站点的 HTTPS 反代实际在宿主机还是容器，找准该域名的 `server` 配置，保留原模型、API、根目录和 TLS 路由。
4. 有旧 Hub 数据就走 [旧版迁移](STANDALONE-DOCKER.zh.md#旧版迁移)，不能重新初始化或另造空卷；目标配置有冲突或服务器余量不足时先报告，不停止原服务腾空间。

## 首次部署

Bot 任务必须显式选 `bot`，不可沿用默认 Station 模式。取得正式 Hub 源码并审阅 `deploy/standalone/install.sh`，然后在**具备公开 HTTPS 与 Docker/Compose 的目标 Linux 主机**运行：

```sh
bash deploy/standalone/install.sh \
  --mode bot \
  --public-url https://relay.example.com/salcara-hub \
  --install-dir /opt/salcara-hub-bot
```

必须把占位域名换成用户指定并控制的实际域名。默认使用包含 Bot 协议的正式 `0.5.3` 镜像，不可回退到 Station `0.5.2`。如果镜像暂不可用，只有在已审阅的 Bot 源码提交完整、工作树干净且目标主机可构建 Docker 镜像时，才显式加 `--source-build`；构建会从 Git 提交归档取源码，并在初始化前运行 Dockerfile 中的全量测试、`vet` 和竞态测试。脚本不修改 Nginx、不安装/重启 Docker、不控制远程 Docker，也不会将初始管理密钥打印出来。

Bot 安装器会验证本地 Hub、公开 HTTPS `/v1/ping`、Bridge SSE 与 App 长轮询，并通过管理 API 创建和保存 `bot-binding.json`。所有检查成功才输出绑定包。可以先配置反代再安装；也可以先运行安装器启动 Hub，再完成以下反代步骤并运行续跑命令。镜像获取和源码构建失败都发生在创建部署目录及初始化数据之前；首次初始化未确认成功时不会给出续跑命令：

- 备份准确站点配置到新建、权限受限的目录，合并两个 Hub location；禁止覆盖整个文件。
- 宿主机 Nginx 使用回环 upstream；容器 Nginx 使用专用私有网络及持久化网络别名，不使用该容器自己的 localhost，不公开 8787。
- 反代要正确支持长连接、`proxy_buffering off`、足够的读超时，以及 `/salcara-hub` 前缀；只对实际部署的主机地址做公网验证。
- 保留 `access_log off`，检查 CDN/WAF 的完整 URL 日志。CDN real IP 只信任明确范围，覆盖而不是追加 `X-Forwarded-For`。
- 在准确代理内先测试配置，成功后只 reload 代理。失败就恢复此次修改，不重启 Docker、Sub2API 或整台服务器。

不要在未授权时直接改 Sub2API 数据库添加菜单。给用户 URL，让用户从自定义 iframe 设置中添加。

### Bot 失败续跑

安装器若在初始化后遇到公网反代暂不可用，会保留安装目录和数据卷，并打印只续跑命令。先修复反代，再在同一台 Linux 主机运行：

```sh
bash /opt/salcara-hub-bot/resume-bot.sh --install-dir /opt/salcara-hub-bot
```

该命令只验证并启动原 Bot 容器，不运行 `-init`、不新建/删除卷、不停止其他服务。源码构建模式会优先检查本地镜像 ID，必要时从权限受限的镜像归档恢复；不会悄悄改拉 Registry 镜像。绑定 JSON 已存在时直接复用；否则从受限管理密钥文件申请新包。若密钥已由用户更换，则停止自动签发，让用户登录管理页生成绑定包。若 AI 云电脑平台不提供公开 HTTPS 入站服务，或无法正确代理 SSE/长轮询，应报告环境不支持 Bot 部署，不安装 VPN/隧道，也不要声称已完成。

## 必须验收并记录

- 运行的是正式 Bot 镜像，或记录了源码提交和本地镜像 ID 的源码构建镜像；Hub 版本 0.5.3，非 root、只读、仅回环发布端口，无 Docker socket。
- 本地 health 正常、公网说明和管理页可达，query 303 清除，同源 iframe 安全头正确；未登录管理 API 返回 401。
- 原 Sub2API 页面和只读健康未受影响；不发送付费模型请求当健康检查。
- 管理页管理密钥登录/更换管理密钥由用户自行输入秘密验收；不要将管理密钥读取到工具输出或要求用户发管理密钥。未做这一步就列为待用户验收。
- 实际手机扫码和原生桌面续聊没有验证时明确待验收，不保证官方宿主未开放的能力。
- 写一份无秘密的部署 MD：镜像/digest、卷名、准确变更/备份位置、只读验证结果、恢复步骤和未验收项。文件留在用户私有工作区，不能上传真实服务器配置。

## 交付给用户的格式

使用部署时的实际 URL 替换以下占位符，所有结果必须来自真实验证：

```text
Hub 容器 / HTTPS：已验收（或说明未完成项）
管理后台：https://relay.example.com/salcara-hub/admin/
自定义 iframe「远程管理」URL：https://relay.example.com/salcara-hub/admin/（管理员可见）
自定义 iframe「远程使用说明」URL：https://relay.example.com/salcara-hub/（普通用户可见）
初始管理密钥文件：/opt/salcara-hub/secrets/admin-initial-login.txt（由用户自己安全读取，勿公开）
文件里只有管理密钥一行，直接复制整行到管理页；首次登录后更换管理密钥，再删除本地初始文件副本。
电脑端站点地址：https://relay.example.com；Bot 部署时电脑和手机都导入同一份绑定 JSON。
待验收：用户登录/更换管理密钥、真实手机绑定和原生桌面续聊（按实际情况填写）。
```

不提供含管理密钥、token、用户名 query 的 URL。管理员菜单可见性不是登录授权；普通用户配对不需要审批。若用户曾把服务器登录密码发在聊天，部署后提醒用户更换，但不擅自更改 SSH、服务器登录密码或访问策略。

交付时附上 [更新与资源](STANDALONE-DOCKER.zh.md#更新与资源)、[停止或卸载](STANDALONE-DOCKER.zh.md#停止或卸载) 的指南。任何升级/卸载均先备份、只操作准确的 Hub 项目，不删除数据卷，不把原 Sub2API 当作 Hub 的附属服务停止。

遗失管理密钥时按 [服务器恢复](STANDALONE-DOCKER.zh.md#管理密钥遗失或需要恢复) 处理：先停止 Hub 并备份，复用同一个数据卷运行 `-reset-admin-key`，新随机密钥只在 `0600` 文件中，不读取到工具输出，不发聊天。重启、保护导出副本并告知用户自行读取即可；绑定/封禁数据保留，不运行首次安装初始化。
