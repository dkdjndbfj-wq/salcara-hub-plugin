# 独立 Docker 部署（0.5.3：管理员账号与自动检查）

> 本文安装目标为 0.5.3，Station 和 Bot 使用同版本镜像。执行前须确认对应正式 Release 和镜像已公开可用；文档更新不表示发布已经完成。0.5.3 的管理员账号、用户档案、按用户登记电脑和单独授权见 [用户与设备管理](USER-MANAGEMENT.zh.md)，需使用配套电脑端。

Hub 独立运行，不修改 Sub2API、不共用它的数据库，也不读取模型 API Key。普通用户电脑与手机扫码配对即可使用，不需要管理员审批；管理员可在后台封禁设备。Docker 只负责消息转发，不能替电脑端绕过 Codex / Claude Desktop 的宿主能力限制。

管理页使用管理员账号和密码登录：首次账号为 `admin`，密码为新部署随机生成的独立管理密钥；从 0.5.0–0.5.2 升级时使用 `admin` 和原管理密钥。登录后可修改账号和密码。旧版管理令牌或 Sub2API 登录 token 不能代替密码。菜单“管理员可见”只控制入口，不能代替后台登录。

## 1. 准备

需要 Linux 主机、可用的 Docker Engine 与 Docker Compose 插件、自己控制的域名及现有 HTTPS 反向代理。安装脚本不会安装 Docker、改 Nginx、重启 Docker daemon、停止 Sub2API 或调整防火墙。

新安装目录与原 Sub2API 目录分开，例如 `/opt/salcara-hub`。Hub 默认项目名 `salcara-hub-standalone`，数据卷 `salcara-hub-standalone_hub-data`，仅监听宿主机 `127.0.0.1:8787`，不要把 8787 公开到互联网。

| 场景 | 采用的步骤 |
| --- | --- |
| 没有运行过独立 Hub | 下面的首次安装 |
| 已有 0.4.0 Hub / 数据卷 | [旧版迁移](#旧版迁移)，不能重新首次安装 |
| 已有 0.5.x 管理密钥版 | [0.5.x 修复更新](#05x-修复更新)，不再初始化 |
| 交给 Agent 部署 | [Agent 部署指南](AGENT-DEPLOY.zh.md) |

默认安装会校验已经公开的正式 Release 和镜像。Station / Bot 均以 [Releases](https://github.com/dkdjndbfj-wq/salcara-hub-plugin/releases) 中的 `standalone-v0.5.3` 为目标，不使用未知 `latest` 或未签名开发产物。目标尚未发布时安装器停止；Bot 可显式选择干净提交源码构建，见下文。两种模式仍使用独立项目和数据卷。

## 2. 首次安装

将示例域名换成自己的域名。以下是本地审阅脚本后执行，不是远程 `curl | sh`。

```sh
git clone --branch standalone-v0.5.3 --depth 1 https://github.com/dkdjndbfj-wq/salcara-hub-plugin.git salcara-hub
cd salcara-hub
less deploy/standalone/install.sh
bash deploy/standalone/install.sh \
  --public-url https://relay.example.com/salcara-hub \
  --install-dir /opt/salcara-hub
```

也可以直接 `bash deploy/standalone/install.sh`，按提示填写 URL 和新安装目录。当前用户必须能访问本机 Docker、写入安装目录的父目录；权限不足时自行选择合适目录或经过授权提权。脚本拒绝已有目录、Hub 容器/数据卷、远程 Docker context 和冲突端口，不覆盖配置、不删除卷、不重复初始化。

脚本的 Station / Bot 版本均固定为 `0.5.3`。它先确认正式 Release 和同版本镜像，再初始化管理密钥，启动并检查对应版本健康。初始管理密钥不会打印、写日志或放进命令参数，只保存到受限文件。任一模式的 Release 或镜像不可用时，默认安装会在创建部署目录和数据卷前停止，不会改用旧版本。失败会保留现场，不显示假完成提示，不自动清理或重试 `-init`。不要同时运行多个安装脚本。

安装结束会给出管理 URL、两种 iframe URL、初始管理密钥文件位置和待配置的反代片段。**容器就绪还不代表公网可用，下一步反代配置不能省略。**

### Bot 中继模式

同一 Hub 内核还支持只做中继的 Bot 部署。Codex、Claude CLI 和其他 Agent 始终运行在各自电脑上，云端不安装模型客户端、不保存模型 API Key，也不提供 VPN 或代理节点。最终验收需要可从电脑和手机访问的公开 HTTPS 反代；脚本会探测 `/v1/ping`、Bridge 流式入口和 App 长轮询入口，任一入口没有正确转发就不会报告安装完成。

Bot 使用独立的 Compose 项目和数据卷，不能把 Station 卷直接改成 Bot。公网 HTTPS、反代路径和流式/长轮询检查均通过后，首次安装器会登录本机 Hub 管理 API，签发绑定 JSON 并保存在安装目录 `bot-binding.json`（权限 `0600`）；安装命令会把这个绑定包输出一次，供电脑和手机导入。初始管理密钥不会输出。若公网反代暂未就绪，安装器保留初始化现场并给出续跑命令，修好反代后运行 `resume-bot.sh`；续跑复用原数据卷，不会重新初始化。已生成的 JSON 会复用，不重复签发。

```sh
bash deploy/standalone/install.sh \
  --mode bot \
  --public-url https://bot.example.com/salcara-hub \
  --install-dir /opt/salcara-hub-bot
```

若 Bot 正式镜像暂不可用，但 Bot 源码已提交到当前 Git 检出，可显式走本机源码构建：

```sh
bash deploy/standalone/install.sh \
  --mode bot \
  --source-build \
  --public-url https://bot.example.com/salcara-hub \
  --install-dir /opt/salcara-hub-bot
```

源码构建要求干净的 Git 工作树，提交中必须包含 Bot 协议和对应测试；构建上下文从该提交归档生成，不包含工作树的额外文件。Dockerfile 会执行完整 Go 测试、`go vet`、竞态测试和运行时构建。目标主机需要访问 Go 基础镜像仓库，并有足够的 Docker 磁盘空间。安装器记录提交 SHA 和镜像 ID，并在权限 `0600` 的部署目录保存镜像归档，供续跑时恢复完全相同的镜像。源码工作树不干净或构建失败时，部署不会创建数据卷或运行初始化。`--source-build` 是显式选择的源码部署，不等同于正式 Registry 发布或签名产物。

若首次初始化已成功、但后续公网通道检查失败，按脚本给出的目录和命令续跑。镜像下载/构建失败和首次初始化未确认时不会输出续跑命令。恢复脚本与 Bot 签发脚本会保存在专用安装目录中：

```sh
bash /opt/salcara-hub-bot/resume-bot.sh --install-dir /opt/salcara-hub-bot
```

不要删除安装目录或数据卷来“重试”。源码部署续跑会验证本地镜像 ID；若 Docker 清理了镜像，则从本地压缩归档恢复，不会改用不同镜像。若所用 AI 云电脑无法提供公开 HTTPS 入口，或不能正确代理 SSE/长轮询，则不能作为 Bot Hub 主机；脚本会停止在未验收状态，不会输出绑定包。已签发的 JSON 也会保存到私有 `bot-binding.json`，恢复时先复用它。管理页仍可查看组状态并撤销邀请。

将同一包分别导入电脑端和手机端后，电脑会生成自己的设备身份并登记到组内，手机会拿到只限该组的凭据并列出组内电脑。每组最多 8 台电脑和 1 部手机；撤销绑定包只停止新增绑定，现有电脑和手机凭据需分别撤销。绑定包不含管理密钥或电脑长期设备密钥，JSON 必须原样通过可信渠道传递。Station 默认模式、旧扫码配对和多 Hub 配置保持不变。

### 不使用安装脚本时

可以手动将仓库根目录的 [compose.release.yml](../compose.release.yml) 复制到一个专用的新目录，并用自己的编辑器创建权限为 `0600` 的 `.env`：

```dotenv
SALCARA_HUB_PUBLIC_URL=https://relay.example.com/salcara-hub
SALCARA_HUB_IMAGE_VERSION=0.5.3
SALCARA_HUB_DISABLE_UPDATES=false
# Bot 沿用同一镜像版本，并设置：
# SALCARA_HUB_MODE=bot、SALCARA_HUB_PROJECT=salcara-hub-bot、
# SALCARA_HUB_DATA_VOLUME=salcara-hub-bot_hub-data
```

先只读确认没有同名项目容器、同名卷、现有安装数据，且 8787 未被占用。然后在该专用目录运行：

```sh
docker compose -f compose.release.yml config --quiet
docker compose -f compose.release.yml pull hub
# 只用于已核实为空的新卷。初始化不会替换既有管理密钥。
docker compose -f compose.release.yml run --rm --no-deps hub -init
docker compose -f compose.release.yml up -d --no-deps hub
docker compose -f compose.release.yml ps
curl --fail http://127.0.0.1:8787/healthz
umask 077
mkdir -m 700 secrets
docker compose -f compose.release.yml cp hub:/data/admin-initial-login.txt ./secrets/admin-initial-login.txt
chmod 600 ./secrets/admin-initial-login.txt
```

任一步失败就停下检查，不能把后续成功命令当成之前步骤也成功。所有 Compose 命令都带 `-f compose.release.yml`，避免误用源码构建配置或另一个项目。生产以固定正式镜像部署，源码 Compose 仅供开发测试。

## 3. 配置 HTTPS 反代

把安装目录中的 `nginx-location.conf`（或仓库的 [同名片段](../deploy/standalone/nginx-location.conf)）合并到**准确的现有域名 HTTPS `server {}`**。先备份该配置，只新增 `/salcara-hub` 两个 location；不替换整个配置，不修改原 `/v1/`、`/api/`、`/` 或 TLS 路由。

确认测试通过才 reload：

```sh
nginx -t
# 仅在上一条成功、且确认为宿主机 Nginx 时执行：
systemctl reload nginx
```

宝塔、1Panel、OpenResty 或容器代理应使用它自己的配置测试和 reload 方法，不照搬宿主机命令，不重启 Sub2API。

- **Nginx 在宿主机**：片段中 `proxy_pass http://127.0.0.1:8787` 可直接使用。
- **Nginx 在容器**：容器自己的 `127.0.0.1` 不是宿主机。让现有代理与 Hub 加入专用私有 Docker 网络，给 Hub 一个不冲突的稳定网络别名，再把该路径 upstream 改为 `http://该别名:8787`。不要开放公网 8787，不让其他不可信容器加入此网络。Compose 需持久保存该外部网络配置，否则重建后连接会丢失；不要只临时执行 `docker network connect` 就声称配置完成。
- **Cloudflare / CDN 前置**：先为 Nginx real IP 模块设置明确可信的 CDN 地址范围，再用片段中的 `X-Forwarded-For $remote_addr` 覆盖转发。不能信任任意公网来源的客户端地址头，否则登录限流可被绕过。

### iframe 与日志保护

Sub2API 0.2.11 的自定义 iframe 会给 URL 自动追加自己的登录 `token` 等参数。只用自己控制的**同源 HTTPS** 页面。Hub 静态页遇到 query 会 303 跳到无参数地址，不把它当 SSO、不交给页面 JS；但这不能撤销前置系统已经记录的 URL。

Hub 的两个 location 必须保留 `access_log off`，还需检查 CDN/WAF、负载均衡及错误追踪是否记录完整 URL。页面使用 `Referrer-Policy: no-referrer`、`Cache-Control: no-store`、`X-Frame-Options: SAMEORIGIN` 与 CSP `frame-ancestors 'self'`。若 iframe 被拦截，只修正对应的同源 CSP 规则，不整体关闭安全头或改成任意来源。

## 4. 登录与添加侧边栏

直接打开管理地址，首次填写账号 `admin`，以初始管理密钥作为密码。密钥在安装输出指出的 `secrets/admin-initial-login.txt` 中，文件只有完整管理密钥一行，复制整行即可。可以用自己的安全编辑器/SFTP 读取，或在自己的安全终端执行安装输出给出的 `cat -- /专用安装目录/secrets/admin-initial-login.txt`。**不要把输出发给 Agent、客服或聊天，不要拼在 URL 中**。

在「系统 → 管理员账号与密码」可更改账号和密码；设置后必须使用新的匹配账号，不能继续省略账号登录。数据卷 `/data/admin-account.json` 保存带随机盐的 PBKDF2 密码哈希，权限 `0600`；`/data/admin-initial-login.txt` 只是初始随机管理密钥文件，更换成功后会删除。安装导出的本地副本不会被网页远程删除，更换后请自行删除这份旧副本。登录使用 HTTPS 安全会话 Cookie，有效期 12 小时，Hub 重启需要重新登录；更改账号或密码会使旧会话失效。

在 Sub2API 系统设置 → 自定义 iframe 页面添加以下 URL。安装脚本/Agent 部署结束也会把**替换成你的实际域名**后的 URL 单独列出来：

| 名称 | 页面 URL | 可见范围 |
| --- | --- | --- |
| 远程管理 | `https://relay.example.com/salcara-hub/admin/` | 管理员 |
| 远程使用说明 | `https://relay.example.com/salcara-hub/` | 普通用户 |

管理菜单直接进入管理界面，未登录时显示账号和密码输入。普通用户说明页不需要管理员登录；用户扫码绑定也不需要审批。普通用户直接访问管理 URL 仍须登录，不会获得全站管理权限。

电脑端填写站点根地址（例如 `https://relay.example.com`），连接后生成二维码，手机扫码绑定。手机/电脑使用独立设备配对凭据，不把管理密钥或模型 Key 当作设备配对凭据。

新电脑第一次登记前，管理员需要在管理页的“新电脑登记”区域生成一次性登记码，并通过可信渠道交给电脑使用者；登记码 24 小时有效且只能登记一台电脑。电脑端在“连接这台电脑”的高级选项中填写登记码，登记成功后会从本机配置中清除。已登记电脑重连不需要新登记码；普通 API Key 账号登记方式不变。

## 5. 部署后自检

```sh
cd /opt/salcara-hub
docker compose -f compose.release.yml ps
curl --fail http://127.0.0.1:8787/healthz
curl --fail https://relay.example.com/salcara-hub/
docker stats --no-stream
```

检查本地健康中的产品为 `salcara-hub-standalone`、版本为 `0.5.3`；确认公网管理页能登录、更改账号和密码、退出，未登录管理 API 返回 401，普通说明页正常，同源 iframe 可打开，原 Sub2API 页面和只读健康接口不变。不要发送付费模型请求当作部署健康检查，不把管理密钥打印在测试报告中。

若是 Agent 部署，只提供成功核实的项目、容器、镜像/版本、反代变更与 URL；没有实测手机扫码/桌面续聊就明确“待设备端验收”。服务器自检通过不能保证电脑端所有宿主能力已可用。

## 0.5.x 修复更新

0.5.0–0.5.2 已使用管理密钥的部署升级到 0.5.3 时不重新初始化；首次以 `admin` 和原管理密钥登录。先备份原项目配置和完整持久卷，在管理页检查更新可升级 Hub 子程序；首次获得后台自动检查还须升级完整镜像。确认 0.5.3 已正式发布后，在原项目的 `.env` 中将 `SALCARA_HUB_IMAGE_VERSION` 改成 `0.5.3`，保留原项目名、卷名、网络及 URL，然后逐条运行：

```sh
docker compose -f compose.release.yml config --quiet
docker compose -f compose.release.yml pull hub
docker compose -f compose.release.yml up -d --no-deps hub
docker compose -f compose.release.yml ps
curl --fail http://127.0.0.1:8787/healthz
```

任一步失败就停止检查。不要运行首次安装脚本、`-init` 或 `down -v`；更新会短暂断开远程并使后台登录会话失效，管理密钥和设备绑定保留。

## 旧版迁移

0.4.0 的认证变更必须通过 **0.5.x 完整镜像升级**（本文目标为 0.5.3，执行前确认已发布）。不要在 0.4.0 管理页点击应用更新来迁移，不要为升级创建另一套项目或空卷，也不要重跑首次安装脚本。

1. 找到原 Hub 的准确 Compose 项目目录、文件、项目名及数据卷。若曾使用 `-p` 自定义项目名，下面所有命令沿用原项目名；不要按示例名称猜测。
2. 记录旧容器镜像 ID/digest、运行 Hub 版本，确认目标 Release/镜像正式可用。备份原 Compose、`.env` 和反代配置。停止的只能是这个 Hub，不停止 Sub2API、数据库、Redis 或 Docker daemon。
3. 停止 Hub 后备份完整 `/data` 到一个尚不存在的新受限目录（含设备、封禁、旧令牌及更新记录），再改配置。不要复制正在写入的数据卷后声称一致性备份完成。

在原项目目录，以正式镜像配置为例（源码或自定义部署请先核对实际文件/项目）：

```sh
# 备份目录必须全新且只供当前管理员访问；示例名字请换成自己的。
umask 077
mkdir -m 700 ../hub-backup-before-0.5.3
cp compose.release.yml .env ../hub-backup-before-0.5.3/
docker compose -f compose.release.yml stop hub
docker compose -f compose.release.yml cp hub:/data ../hub-backup-before-0.5.3/data
chmod -R go-rwx ../hub-backup-before-0.5.3
```

备份失败时停止升级，在原配置未改变的前提下 `docker compose -f compose.release.yml start hub` 恢复旧 Hub。不要删卷或猜测性继续。

4. 用自己的编辑器将正式 Compose 更新成 0.5.3 的配置，保留原项目名、卷名和网络，`.env` 中设 `SALCARA_HUB_IMAGE_VERSION=0.5.3`。新的配置不再使用 `SALCARA_HUB_ADMIN_TOKEN_FILE`。下面在**已完成完整备份且旧 Hub 已停止**后运行：

```sh
docker compose -f compose.release.yml config --quiet
docker compose -f compose.release.yml pull hub
# 此次仅给旧卷新增管理密钥文件；不会删改电脑配对数据。
# 若已有 admin-account.json，应停止核对，不要覆盖或再次初始化。
docker compose -f compose.release.yml run --rm --no-deps hub -init
docker compose -f compose.release.yml up -d --no-deps hub
docker compose -f compose.release.yml ps
curl --fail http://127.0.0.1:8787/healthz
```

5. 按首次部署的安全方式导出 `admin-initial-login.txt` 到一个新的 `0700` 目录，登录并更换管理密钥，按上面的自检逐项核实。电脑配对、封禁和资源模式数据保留；旧 `admin-token` 即使留在备份/旧卷也不再用于登录，不能把它当管理密钥。

容器替换会短暂断开远程连接，在途请求可能失败；电脑本地任务通常继续，但不承诺所有请求零中断。迁移失败时保留现场，只在确认兼容性和备份有效后恢复旧镜像及必要数据；旧版管理令牌鉴权会随旧程序恢复。不要删除 `/data/updates` 或强行修改更新状态来降级。认证版上线前应先在副本卷测试升级/回退。

## 管理密钥遗失或需要恢复

仍能登录时，直接使用管理页「系统 → 管理员账号与密码」，不需要另外部署后台。完全遗失时，只有能操作服务器 Docker 的部署者才能恢复，不提供公网免登录重置接口。

先按上面的备份流程确认准确项目、停止**仅 Hub**并备份完整数据。使用已经升级到 0.5.x 的正式镜像，在原项目目录逐条运行；任何一步失败立即停下检查：

```sh
docker compose -f compose.release.yml stop hub
# 先完成受限目录中的完整配置 /data 备份，再执行下一条。
docker compose -f compose.release.yml run --rm --no-deps hub -reset-admin-key
```

恢复会随机生成新的独立密钥，仅保存为 `/data/admin-initial-login.txt` 中完整的一行（`0600`），并将主管理员账号恢复为 `admin`；不打印密码内容。它保留电脑绑定、用户、封禁和资源模式数据，不删除卷；不要运行首次安装脚本或 `-init` 来恢复。

恢复后启动 Hub，再安全导出新密钥。以下假定首次安装已创建受限 `secrets` 目录；先确认目录及目标文件由本人控制且不是符号链接。旧本地副本会被新密钥覆盖，不能继续复制旧副本登录：

```sh
docker compose -f compose.release.yml up -d --no-deps hub
test -d ./secrets && test ! -L ./secrets
test ! -L ./secrets/admin-initial-login.txt
chmod 700 ./secrets
umask 077
docker compose -f compose.release.yml cp hub:/data/admin-initial-login.txt ./secrets/admin-initial-login.txt
chmod 600 ./secrets/admin-initial-login.txt
```

在自己的安全编辑器/终端读取新密钥，以 `admin` 和新密钥登录；不要发给 Agent 或聊天。重启后旧登录会话失效，旧密钥不再有效。确认登录后可以在后台更改账号和密码，并自行删除这份本地初始副本。恢复失败时保留现场和备份，不删除卷或猜测性重复操作。

## 更新与资源

0.5.3 新增独立的「版本更新」导航。使用包含新版启动器的 Docker 镜像时，启动器在服务启动完成后立即检查签名更新源，默认每 6 小时再次检查；关闭浏览器后仍继续检测。左上角显示当前版本，发现新版后版本号高亮，点击即可打开更新确认。更新页面显示当前版本、可用版本、发布说明、最近检查和下次检查时间。可随时点击「检查更新」，或通过「一键更新」安装；后台检测不会自动安装。

默认更新源和发布者公钥已内置，标准 Compose 部署无需另装更新服务或挂载 Docker socket。两个 Compose 文件支持在 `.env` 中设置：

| 配置 | 默认值 | 说明 |
| --- | --- | --- |
| `SALCARA_HUB_UPDATE_CHECK_INTERVAL` | `6h` | 后台检测间隔，接受 `5m` 至 `168h` 的 Go duration，例如 `30m`、`12h`；手动检查后重新安排下次检测 |
| `SALCARA_HUB_DISABLE_UPDATES` | `false` | 设为 `true` 同时关闭自动检测和应用更新 |

修改 `.env` 后，在原项目目录执行 `docker compose -f compose.release.yml up -d --no-deps hub` 使配置生效。更新源不可达、清单无效或签名验证失败时显示检测失败，现有 Hub 继续运行。

一键更新安装**签名 Hub 应用程序**。先验固定发布者 Ed25519 签名、产品、大小/哈希及协议兼容性，健康失败回退程序；程序回退不等于数据回滚。更新会短暂断开远程连接，保留管理密钥、设备绑定和持久数据，管理员需要重新登录查看最终版本。Docker 镜像、启动器和 Sub2API 不在网页更新范围内。

已有安装**首次启用后台自动检测，需要换用包含新启动器的已发布镜像**；单独在旧版网页更新 Hub 子程序不能升级启动器。历史 `0.5.2` 镜像没有这项后台检测能力；目标 `0.5.3` 的公开可用性以实际发布为准。底层镜像/启动器仍按 Release 说明在原项目目录完整备份、拉固定镜像并 `up -d --no-deps hub`，不使用未知 `latest`，不执行 `down -v`。

这是**一个 Hub 容器**，内有小型 Go 启动器与 Hub，共享容器资源限制；没有另一套 Sub2API、Redis、数据库、Node 或 Docker 更新器，不挂 Docker socket。默认内存硬限制 384 MiB、CPU 配额 1 核、进程上限 64。后台“省资源 / 均衡 / 高负载”对应 Hub Go 软目标 96 / 192 / 320 MiB，不改变 Docker 硬限制，也不是实测占用或用户数量承诺。

手机按需收发；电脑为接收 NAT 后任务仍有轻量连接。旧版进度主要在内存中有限暂存；0.5.3 源码已实现加密持久消息、结果及图片分片，默认未确认内容保留 7 天，可在系统页调整为 1–30 天。电脑离线时暂存新消息默认关闭，开启后最长等待 10 分钟；执行结果未知的任务不自动重复执行。详情见 [消息存储实现](DURABLE-REMOTE-MAILBOX-IMPLEMENTATION-20261107.zh.md)。这仍是有配额与保留期限的暂存，不是永久云端聊天备份。HTTPS 保护传输，当前不是端到端加密，站点运营者能读取转发内容。

0.5.3 源码新增的独立运维密钥、审计导出、监控告警接口和离线加密备份工具见 [协作管理与恢复](OPERATIONS-AND-RECOVERY.zh.md)。备份包含完整 Hub 数据目录；反代、FCM、Compose/env 和独立 Edge 数据需要另外备份。恢复只能写入新目录，先演练验证再切换生产卷。

排查可用 `docker compose -f compose.release.yml logs --tail 100 hub`，先检查是否含私人信息，不公开整份日志、`.env`、管理密钥文件或完整数据备份。真实内存以 `docker stats --no-stream` 为准，也要给现有服务、反代和系统留下余量。

## 停止或卸载

先找到准确的原 Hub Compose 项目及卷，按旧版迁移中的步骤停止**仅 Hub**并备份部署配置、完整 `/data` 和反代片段到全新受限目录。备份失败不要继续。备份包含管理密钥哈希、设备秘密等，不能公开。

- 临时停用：在原项目目录执行 `docker compose -f compose.release.yml stop hub`；恢复用 `start hub`。
- 卸载服务：备份成功后，在准确原项目目录执行 `docker compose -f compose.release.yml down`，**不带 `-v`**。这只移除该 Hub 项目的容器/项目网络，保留数据卷；不要停其他项目，不用 `docker system prune`。
- 只删除此前新增的两个 Hub location，测试 Nginx 配置成功后 reload；保留原站全部其他路由。随后从 Sub2API 自定义菜单移除这两个入口。

本指南不自动删除数据卷、备份、密钥文件或整份部署目录。彻底清理或重装必须由部署者复核准确目标并确认备份，不能把保留着旧数据的情况当作首次空卷安装。
