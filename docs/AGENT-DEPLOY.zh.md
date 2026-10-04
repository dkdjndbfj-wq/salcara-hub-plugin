# 给 Agent 的独立 Hub 部署指南

只部署 Salcara Hub，不修改桌面/手机 UI，不操作用户模型 Key，不把 CLI 验收说成原生桌面能力验收。遵循用户授权；不得把服务器密码、管理密钥、设备秘密、原站 JWT、私钥或真实配置提交 GitHub、写入公开报告或发到聊天。

## 部署前只读检查

1. 确认 SSH 主机身份、系统/架构、内存和磁盘余量；密码仅通过不回显登录输入，不写命令参数或文件。
2. 检查本机 Docker/Compose、容器名/镜像/端口、Hub 数据卷和目标安装目录。不要全量输出容器 Env、`.env`、数据库密码或日志。
3. 确认站点的 HTTPS 反代实际在宿主机还是容器，找准该域名的 `server` 配置，保留原模型、API、根目录和 TLS 路由。
4. 有旧 Hub 数据就走 [旧版迁移](STANDALONE-DOCKER.zh.md#旧版迁移)，不能重新初始化或另造空卷；目标配置有冲突或服务器余量不足时先报告，不停止原服务腾空间。

## 首次部署

取得正式 `standalone-v0.5.0` 源码并审阅 `deploy/standalone/install.sh`，在目标 Linux 主机运行：

```sh
bash deploy/standalone/install.sh \
  --public-url https://relay.example.com/salcara-hub \
  --install-dir /opt/salcara-hub
```

必须把占位域名换成用户指定并控制的实际域名。只使用固定正式 0.5.0 镜像；Release 或镜像不可达就停止，不能输出“已经部署完成”。脚本不修改 Nginx、不安装/重启 Docker、不控制远程 Docker，也不会将初始管理密钥打印出来。

脚本健康检查成功后，仍要完成反代步骤：

- 备份准确站点配置到新建、权限受限的目录，合并两个 Hub location；禁止覆盖整个文件。
- 宿主机 Nginx 使用回环 upstream；容器 Nginx 使用专用私有网络及持久化网络别名，不使用该容器自己的 localhost，不公开 8787。
- 保留 `access_log off`，检查 CDN/WAF 的完整 URL 日志。CDN real IP 只信任明确范围，覆盖而不是追加 `X-Forwarded-For`。
- 在准确代理内先测试配置，成功后只 reload 代理。失败就恢复此次修改，不重启 Docker、Sub2API 或整台服务器。

不要在未授权时直接改 Sub2API 数据库添加菜单。给用户 URL，让用户从自定义 iframe 设置中添加。

## 必须验收并记录

- 运行的是正式镜像，Hub 版本 0.5.0，非 root、只读、仅回环发布端口，无 Docker socket。
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
电脑端站点地址：https://relay.example.com；电脑生成二维码，手机扫码绑定。
待验收：用户登录/更换管理密钥、真实手机绑定和原生桌面续聊（按实际情况填写）。
```

不提供含管理密钥、token、用户名 query 的 URL。管理员菜单可见性不是登录授权；普通用户配对不需要审批。若用户曾把服务器登录密码发在聊天，部署后提醒用户更换，但不擅自更改 SSH、服务器登录密码或访问策略。

交付时附上 [更新与资源](STANDALONE-DOCKER.zh.md#更新与资源)、[停止或卸载](STANDALONE-DOCKER.zh.md#停止或卸载) 的指南。任何升级/卸载均先备份、只操作准确的 Hub 项目，不删除数据卷，不把原 Sub2API 当作 Hub 的附属服务停止。

遗失管理密钥时按 [服务器恢复](STANDALONE-DOCKER.zh.md#管理密钥遗失或需要恢复) 处理：先停止 Hub 并备份，复用同一个数据卷运行 `-reset-admin-key`，新随机密钥只在 `0600` 文件中，不读取到工具输出，不发聊天。重启、保护导出副本并告知用户自行读取即可；绑定/封禁数据保留，不运行首次安装初始化。
