# 独立 Docker 部署（0.5.6 发布目标）

> 本文脚本安装目标为 **0.5.6**，Station 和 Bot 使用同版本镜像；本次配套 Android 1.8.6（versionCode 27）/ Desktop（Bridge）1.6.9，发布签名身份与最低准入版本策略不变。所有安装、升级命令须在对应正式 Release、公开镜像及下载校验确认后执行；文档或下一版源码修改本身不是发布凭证。0.5.3 引入的管理员账号、用户档案、按用户登记电脑和单独授权见 [用户与设备管理](USER-MANAGEMENT.zh.md)。0.5.6 的任务恢复、待发撤回、Bot 清理修复及待验收范围见 [版本说明](RELEASE-0.5.6.md)。

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

默认安装会校验已经公开的正式 Release 和镜像。Station / Bot 均以 [Releases](https://github.com/dkdjndbfj-wq/salcara-hub-plugin/releases) 中的 `standalone-v0.5.6` 为目标，不使用未知 `latest` 或未签名开发产物。若对应正式资产尚未公开或不可用，安装器停止；Bot 可显式选择干净提交源码构建，见下文。两种模式仍使用独立项目和数据卷。

## 2. 首次安装

先确认 0.5.6 的正式标签、Release 和镜像已公开可用，再将示例域名换成自己的域名。以下是本地审阅脚本后执行，不是远程 `curl | sh`。

```sh
git clone --branch standalone-v0.5.6 --depth 1 https://github.com/dkdjndbfj-wq/salcara-hub-plugin.git salcara-hub
cd salcara-hub
less deploy/standalone/install.sh
bash deploy/standalone/install.sh \
  --public-url https://relay.example.com/salcara-hub \
  --install-dir /opt/salcara-hub
```

也可以直接 `bash deploy/standalone/install.sh`，按提示填写 URL 和新安装目录。当前用户必须能访问本机 Docker、写入安装目录的父目录；权限不足时自行选择合适目录或经过授权提权。脚本拒绝已有目录、Hub 容器/数据卷、远程 Docker context 和冲突端口，不覆盖配置、不删除卷、不重复初始化。

脚本的 Station / Bot 版本均固定为 `0.5.6`。它先确认正式 Release 和同版本镜像，再初始化管理密钥，启动并检查对应版本健康。初始管理密钥不会打印、写日志或放进命令参数，只保存到受限文件。任一模式的 Release 或镜像不可用时，默认安装会在创建部署目录和数据卷前停止，不会改用旧版本。失败会保留现场，不显示假完成提示，不自动清理或重试 `-init`。不要同时运行多个安装脚本。

安装结束会给出管理 URL、两种 iframe URL、初始管理密钥文件位置和待配置的反代片段。**容器就绪还不代表公网可用，下一步反代配置不能省略。**

### Bot 中继模式

同一 Hub 内核还支持只做中继的 Bot 部署。Codex、Claude CLI 和其他 Agent 始终运行在各自电脑上，云端不安装模型客户端、不保存模型 API Key，也不提供 VPN 或代理节点。实际使用需要电脑和手机都能访问的公开 HTTPS 反代。安装/续跑脚本只做本地健康、HTTPS `/v1/ping`，以及无凭据虚拟设备请求返回 `400/403` 的基础路由检查；**它们不测试已认证 SSE、长轮询持续等待、反代无缓冲或真实任务往返**，通过不等于任务已可用。

Bot 使用独立的 Compose 项目和数据卷，不能把 Station 卷直接改成 Bot。基础检查通过后，首次安装器可调用管理 API，签发绑定 JSON 并保存在安装目录 `bot-binding.json`（权限 `0600`）。绑定脚本**只输出私有文件路径**，绝不展示 JSON、`invite` 或 `signature`；用户通过可信私有文件渠道把同一包导入电脑和手机，不能粘贴到聊天、公开报告或日志。初始管理密钥也不会输出。若公网反代暂未就绪，安装器保留初始化现场并给出续跑命令，修好反代后运行 `resume-bot.sh`；续跑复用原数据卷，不会重新初始化。

安装/续跑还会保存 `bot-deployment-checks.json`，其 `scope` 为 `"unauthenticated-public-route-only"`，`authenticatedSSE`、`authenticatedLongPoll`、`proxyBuffering`、`taskRoundTrip` 全部为 `"not_tested"`，`readyForTasks=false`。这是有限范围的部署检查记录，不是端到端（E2E）验收凭证；不能因为绑定包已签发或脚本退出成功就改称“全链路可用”。详见 [分层验收](#bot-分层验收与用户设备检查)。

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

不要删除安装目录或数据卷来“重试”。源码部署续跑会验证本地镜像 ID；若 Docker 清理了镜像，则从本地压缩归档恢复，不会改用不同镜像。若所用 AI 云电脑没有公开 HTTPS 入口，基础路由检查不能完成；即使检查通过，仍需用户验证代理能否实际维持 SSE/长轮询，不能从 `400/403` 推定支持长连接或无缓冲。

同 URL 的已有 `bot-binding.json` 会在文件类型、权限和本地结构检查通过后复用，只输出路径，不自动再签。**这个本地检查不验证签名、不查询邀请是否有效/已撤销/已满额，也不证明已有组可用。** 若初始管理密钥已更换，自动登录失败时由用户在管理页处理，不把新密钥交给 Agent。

### 邀请签发结果不确定时

邀请 POST 可能在服务器成功、但响应未到达或文件未保存；不能把超时当成“未签发”。脚本会在安装目录保留权限 `0600`、不含邀请秘密的 `.bot-binding-pending` 标记，阻止普通安装/续跑自动再次签发。保留该状态，按以下顺序处理：

1. 管理员自行登录管理页检查 Bot 组、邀请和现有设备。若能从已有可信私有副本取得已签发的原包，可将其原样保存为原安装目录的私有 `bot-binding.json`，再按正常续跑流程复用，不新签；不要假定管理页能重新导出原邀请。
2. 若无法取得原包，先在管理页查清并撤销结果不明的旧邀请；撤销邀请只阻止新增绑定，已有电脑/手机凭据需单独核对、按需要撤销。
3. 只有已完成核查和撤销、确需新邀请且没有已保存绑定包时，才由管理员显式确认并执行：

```sh
bash /opt/salcara-hub-bot/create-bot-binding.sh \
  --install-dir /opt/salcara-hub-bot \
  --confirm-prior-invite-revoked
```

该选项只是**操作者对已完成核查/撤销的确认**，不是脚本自动证明旧邀请已撤销；普通 `install.sh` / `resume-bot.sh` 不会传入此选项。管理员无法确认结果时保持阻断。不要直接删除标记、删包重试、创建空数据卷或重新 `-init` 绕过保护。恢复后的新签发仍只输出私有文件路径。

将同一包分别导入电脑端和手机端后，电脑会生成自己的设备身份并登记到组内，手机会拿到只限该组的凭据并列出组内电脑。每组最多 8 台电脑和 1 部手机；撤销绑定包只停止新增绑定，现有电脑和手机凭据需分别撤销。绑定包不含管理密钥或电脑长期设备密钥，JSON 必须原样通过可信渠道传递。Station 默认模式、旧扫码配对和多 Hub 配置保持不变。

### Bot 数据与身份恢复

迁移或重装必须保留**完整 DataDir**（Docker 内 `/data`），而不是只保存绑定包或某个 seed。停下准确的 Hub 项目后制作一致性私有备份，范围至少包括：

| 持久状态 | 必须保留的内容 |
| --- | --- |
| `bot-groups.json` | Bot 组、邀请摘要和撤销状态、电脑成员、手机身份/凭据摘要、手机代次及重置恢复元数据、Bot 签发密钥 |
| `bot-phone-intents.json` | 待完成的手机撤销/重置意图，与主状态共同恢复；不可删掉或单独漏备 |
| `devices.json` | 电脑设备身份、设备秘密摘要、绑定/配对状态和封禁记录 |
| `ingress-registry.json` | Hub 实例身份、签名 seed、canonical 地址、revision 和入口配置 |
| DataDir 中其他全部状态 | 管理凭据、消息/通知数据库及关联文件、加密密钥、更新记录等；不要仅按上面列出的文件挑选备份 |

Compose/env、反代和 TLS 等数据目录外配置另行备份。手机和电脑本机的绑定凭据、身份 pin 也要保留，服务端的凭据摘要不能还原丢失的客户端秘密。备份不能进入 Git、公开报告或聊天。

按 [协作管理与恢复](OPERATIONS-AND-RECOVERY.zh.md#恢复演练) 先恢复到新的受限目录/副本卷，验证后再切换准确的生产挂载；不重新初始化，不让旧卷和克隆卷同时作为同一站点服务。旧备份可能带回当时尚未撤销的凭据，切换前需核对当前撤销情况。空卷会产生新身份；保留一个 seed 或旧邀请包不能补回原组、手机和电脑数据。

0.5.5 的手机撤销/重置尚未安全保存时会返回可重试状态，不会假报完成。必须保留原客户端恢复凭据、完整服务器状态及意图日志；存在待完成状态时，不得直接降级到不理解该日志的旧 Hub，也不能删除日志或还原旧备份绕过撤销。程序回退前须核对数据兼容性和最新撤销事实。

地址变化还须区分两种流程：

- **新设备首次加入**：旧 v1 绑定 JSON 的签名包含 canonical `hubUrl`，不能手工改地址、邀请或签名。新地址的首次邀请必须由管理端重新签发；部署脚本调用的创建邀请端点会新建组。管理员另可通过原组 `phone/reset` 显式确认、核对版本并幂等重置手机后重新签发，但会撤销旧手机凭据并更换邀请，不是无影响的地址刷新。已有组和设备正常迁址只走同身份恢复，不调用 reset 或再次 bind；安装/续跑脚本不会自动重置手机。
- **已绑定设备恢复**：保留原客户端身份 pin 和绑定凭据，验证原 Hub 公钥签名的入口清单、Hub 身份、有效期及新的 nonce 证明，并确认候选完整 URL 是签名清单中的精确成员后，才允许向新地址发送原凭据。仅新站能响应同公钥挑战还不够；不做第二次 bind、不清空 pin、不把旧 secret 发送给未验证的新实例。身份不匹配时停止核查。完整数据恢复后修改 canonical 仍需重新核验入口；不能把 Hub 侧恢复测试当成真实客户端已经恢复。

没有稳定域名、可达的已知入口或仍有效的受信入口缓存，客户端不能凭空发现服务器的新 IP。管理员必须通过可信渠道提供新地址，再按上述同身份流程恢复；稳定地址的 DNS/反代变更也要实际核对。脚本不会提供自动跨站迁移或未经授权的隧道。

### 不使用安装脚本时

可以手动将仓库根目录的 [compose.release.yml](../compose.release.yml) 复制到一个专用的新目录，并用自己的编辑器创建权限为 `0600` 的 `.env`：

```dotenv
SALCARA_HUB_PUBLIC_URL=https://relay.example.com/salcara-hub
SALCARA_HUB_IMAGE_VERSION=0.5.6
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

新电脑使用可信 API 自动登记时，站点须事先配置明确受信的登记服务，并授权相应 API 来源与分组；桌面端验证资格后向 Hub 提交短效签名授权，模型 API Key 不交给 Hub。未配置该能力的独立 Hub 仍由管理员在“新电脑登记”区域生成一次性登记码，通过可信渠道交给电脑使用者；登记码 24 小时有效且只能登记一台电脑。电脑端在“连接这台电脑”的高级选项中填写登记码，登记成功后从本机配置中清除。已登记电脑用原设备身份重连，不需要再次提交 API Key 或新登记码。详见 [可信 API 电脑登记](API-DEVICE-ENROLLMENT.md)。

## 5. 部署后自检

```sh
cd /opt/salcara-hub
docker compose -f compose.release.yml ps
curl --fail http://127.0.0.1:8787/healthz
curl --fail https://relay.example.com/salcara-hub/
docker stats --no-stream
```

检查本地健康中的产品为 `salcara-hub-standalone`、版本为 `0.5.6`；确认公网管理页能登录、更改账号和密码、退出，未登录管理 API 返回 401，普通说明页正常，同源 iframe 可打开，原 Sub2API 页面和只读健康接口不变。核对首页、状态页均显示配套 Android 1.8.6 / Bridge 1.6.9，并在亮色、暗色和手机宽度下检查布局。不要发送付费模型请求当作部署健康检查，不把管理密钥打印在测试报告中。

若是 Agent 部署，只提供成功核实的项目、容器、镜像/版本、反代变更与 URL；没有实测手机扫码/桌面续聊就明确“待设备端验收”。服务器自检通过不能保证电脑端所有宿主能力已可用。

### Bot 分层验收与用户设备检查

| 层级 | 安装/续跑能否完成 | 验收边界 |
| --- | --- | --- |
| 本地产品、版本和健康 | 是，实际通过才记录 | 只证明当前 Hub 本地运行正常 |
| HTTPS ping 与无凭据路由 | 是，虚拟设备返回预期 `400/403` | 只证明公开基础入口响应符合预期；不是设备认证测试 |
| 绑定包私有保存/复用 | 可执行；失败或 pending 必须保留状态 | 只有路径可输出；本地结构检查不是签名/有效性验证 |
| 管理登录、页面和原服务 | 需独立核对，登录秘密由用户自行输入 | ping 或绑定包存在不能代替这些检查 |
| 已认证 SSE / 长轮询 | 否，`not_tested` | 需真实用户设备上的持续连接、事件和重连证据 |
| 反代无缓冲 | 否，`not_tested` | 静态 `proxy_buffering off` 或拒绝码不能证明实际持续传输无缓冲 |
| 真实任务往返与原生会话连续性 | 否，`not_tested` | 本次不调用真实 Agent/付费模型，仍需另行授权与用户手动验收 |

用户可在自己控制的测试电脑和手机上，通过可信文件渠道导入绑定包，确认对应组与电脑列表、设备状态，手动检查断线后重连及事件到达。管理密码、设备 secret、邀请和会话正文都不交给 Agent 或测试报告。不要为了获得“通过”而自动发起真实 Agent 任务或付费模型调用；任务往返需另行明确授权，由用户在受控测试会话手动验证并记录实际结果，未做就保持待验收。

`bot-deployment-checks.json` 的四个高层检查始终是 `not_tested`、`readyForTasks=false`，不能手改成端到端通过。真实设备或代理链路另行验收时，保留单独记录，注明设备版本、实际操作和未测项目；不把候选源码、模拟测试或路由检查说成已上线验收。

## 0.5.x 修复更新

已有 0.5.x 部署升级到 0.5.6 时不重新初始化。0.5.3 引入管理员账号：从 0.5.0–0.5.2 首次升级时以 `admin` 和原管理密钥登录；已经设置自定义账号和密码的安装继续使用原账号和密码。先备份原项目配置和完整持久卷，在管理页检查更新可升级 Hub 子程序；首次获得 0.5.3 引入的后台自动检查仍须升级包含新版 launcher 的完整镜像。确认 0.5.6 正式 Release 和镜像已公开可用后，在原项目的 `.env` 中将 `SALCARA_HUB_IMAGE_VERSION` 改成 `0.5.6`，保留原项目名、卷名、网络及 URL，然后逐条运行：

```sh
docker compose -f compose.release.yml config --quiet
docker compose -f compose.release.yml pull hub
docker compose -f compose.release.yml up -d --no-deps hub
docker compose -f compose.release.yml ps
curl --fail http://127.0.0.1:8787/healthz
```

任一步失败就停止检查。不要运行首次安装脚本、`-init` 或 `down -v`；更新会短暂断开远程并使后台登录会话失效，管理密钥和设备绑定保留。

## 旧版迁移

0.4.0 的认证变更必须通过 **0.5.x 完整镜像升级**（本文目标为 0.5.6 发布目标，执行前确认正式 Release 和镜像已公开可用）。不要在 0.4.0 管理页点击应用更新来迁移，不要为升级创建另一套项目或空卷，也不要重跑首次安装脚本。

1. 找到原 Hub 的准确 Compose 项目目录、文件、项目名及数据卷。若曾使用 `-p` 自定义项目名，下面所有命令沿用原项目名；不要按示例名称猜测。
2. 记录旧容器镜像 ID/digest、运行 Hub 版本，确认目标 Release/镜像正式可用。备份原 Compose、`.env` 和反代配置。停止的只能是这个 Hub，不停止 Sub2API、数据库、Redis 或 Docker daemon。
3. 停止 Hub 后备份完整 `/data` 到一个尚不存在的新受限目录（含设备、封禁、旧令牌及更新记录），再改配置。不要复制正在写入的数据卷后声称一致性备份完成。

在原项目目录，以正式镜像配置为例（源码或自定义部署请先核对实际文件/项目）：

```sh
# 备份目录必须全新且只供当前管理员访问；示例名字请换成自己的。
umask 077
mkdir -m 700 ../hub-backup-before-0.5.6
cp compose.release.yml .env ../hub-backup-before-0.5.6/
docker compose -f compose.release.yml stop hub
docker compose -f compose.release.yml cp hub:/data ../hub-backup-before-0.5.6/data
chmod -R go-rwx ../hub-backup-before-0.5.6
```

备份失败时停止升级，在原配置未改变的前提下 `docker compose -f compose.release.yml start hub` 恢复旧 Hub。不要删卷或猜测性继续。

4. 用自己的编辑器将正式 Compose 更新成已发布的 0.5.6 配置，保留原项目名、卷名和网络，`.env` 中设 `SALCARA_HUB_IMAGE_VERSION=0.5.6`。新的配置不再使用 `SALCARA_HUB_ADMIN_TOKEN_FILE`。下面在**已完成完整备份且旧 Hub 已停止**后运行：

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

发布 GitHub Release、镜像或签名更新清单不会自动重启已有服务器。公开首页与状态页的配套版本嵌入 Hub 构建；只更新手机或 Bridge、或只发布源码，都不会改变线上页面。部署者须在准确目标完成升级后，另行检查 `/healthz`、首页和 `/salcara-hub/status` 的实际版本。本版不调整最低准入版本或启用强制更新。

已有安装**首次启用后台自动检测，需要换用包含新启动器的已发布镜像**；单独在旧版网页更新 Hub 子程序不能升级启动器。历史 `0.5.2` 镜像没有这项后台检测能力，`0.5.3` 引入该能力；本文目标 `0.5.6` 仍须确认正式 Release 和镜像公开可用后才能安装。底层镜像/启动器仍按 Release 说明在原项目目录完整备份、拉固定镜像并 `up -d --no-deps hub`，不使用未知 `latest`，不执行 `down -v`。

这是**一个 Hub 容器**，内有小型 Go 启动器与 Hub，共享容器资源限制；没有另一套 Sub2API、Redis、数据库、Node 或 Docker 更新器，不挂 Docker socket。默认内存硬限制 384 MiB、CPU 配额 1 核、进程上限 64。后台“省资源 / 均衡 / 高负载”对应 Hub Go 软目标 96 / 192 / 320 MiB，不改变 Docker 硬限制，也不是实测占用或用户数量承诺。

手机按需收发；电脑为接收 NAT 后任务仍有轻量连接。旧版进度主要在内存中有限暂存；0.5.3 源码已实现加密持久消息、结果及图片分片，默认未确认内容保留 7 天，可在系统页调整为 1–30 天。电脑离线时暂存新消息默认关闭，开启后最长等待 10 分钟；执行结果未知的任务不自动重复执行。详情见 [消息存储实现](DURABLE-REMOTE-MAILBOX-IMPLEMENTATION-20261107.zh.md)。这仍是有配额与保留期限的暂存，不是永久云端聊天备份。HTTPS 保护传输，当前不是端到端加密，站点运营者能读取转发内容。

0.5.6 为尚未派发的待发任务增加持久撤回，必须由 Hub 确认后才算成功；停止等待或断开手机连接不等于停止 Agent。正文到期不抹去“结果未知”“结果截断”或“已撤回”的事实。到期、损坏且确认未派发的队列记录会安全结束并释放正文；管理页另显示防重复执行记录的数量与上限。清理正文不会删除这些安全身份，容量满时需维护人员处理，不能靠清空数据或自动换任务编号重试。

0.5.3 源码新增的独立运维密钥、审计导出、监控告警接口和离线加密备份工具见 [协作管理与恢复](OPERATIONS-AND-RECOVERY.zh.md)。备份包含完整 Hub 数据目录；反代、FCM、Compose/env 和独立 Edge 数据需要另外备份。恢复只能写入新目录，先演练验证再切换生产卷。

排查可用 `docker compose -f compose.release.yml logs --tail 100 hub`，先检查是否含私人信息，不公开整份日志、`.env`、管理密钥文件或完整数据备份。真实内存以 `docker stats --no-stream` 为准，也要给现有服务、反代和系统留下余量。

## 停止或卸载

先找到准确的原 Hub Compose 项目及卷，按旧版迁移中的步骤停止**仅 Hub**并备份部署配置、完整 `/data` 和反代片段到全新受限目录。备份失败不要继续。备份包含管理密钥哈希、设备秘密等，不能公开。

- 临时停用：在原项目目录执行 `docker compose -f compose.release.yml stop hub`；恢复用 `start hub`。
- 卸载服务：备份成功后，在准确原项目目录执行 `docker compose -f compose.release.yml down`，**不带 `-v`**。这只移除该 Hub 项目的容器/项目网络，保留数据卷；不要停其他项目，不用 `docker system prune`。
- 只删除此前新增的两个 Hub location，测试 Nginx 配置成功后 reload；保留原站全部其他路由。随后从 Sub2API 自定义菜单移除这两个入口。

本指南不自动删除数据卷、备份、密钥文件或整份部署目录。彻底清理或重装必须由部署者复核准确目标并确认备份，不能把保留着旧数据的情况当作首次空卷安装。
