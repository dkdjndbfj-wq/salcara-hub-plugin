# 独立 Docker 部署：不修改 Sub2API

推荐新部署使用独立 Salcara Hub 容器。它和 Sub2API 是两个服务，只在同一个 HTTPS 域名的不同路径提供入口：原模型请求继续走原来的 `/v1/`，设备配对、远程消息和管理界面走 `/salcara-hub/`。Sub2API 不需要宿主补丁、不需要安装 `.s2plugin`，可以按它自己的官方升级流程更新。

这是新增的独立运行方式，不是把已发布的 v0.3.1 插件变成官方兼容插件。v0.3.1 插件仍只适用于文档指定的定制宿主。不要同时将两个 Hub 挂到相同路径，也不要让两个进程写同一个 Hub 数据目录。**这里没有把 Codex / Claude Desktop 的桌面控制限制变成已解决；这些能力仍取决于电脑端适配和授权。**

## 架构与权限

```text
手机 / 电脑 ── HTTPS ── 现有 Nginx
                         ├── /v1/、/api/、/ → 原 Sub2API（保持原配置）
                         └── /salcara-hub/  → 127.0.0.1:8787 → 独立 Hub
Sub2API 自定义侧边栏 ── 同源 iframe → Hub 管理页或使用说明页
```

Hub 仅接受独立设备配对凭据，不验证 Sub2API 用户资格、不请求模型接口、不读取模型 API Key、不共用 Sub2API 数据库。管理员使用 Hub 自己的新随机管理令牌。Sub2API 的“管理员可见”只控制菜单入口，不是 Hub API 的管理员授权，也不是免登录 SSO。

HTTPS 保护传输，但当前不是端到端加密；站点运营者能读取转发内容，用户需要信任站点。服务器缓存不是永久聊天备份。电脑原会话、项目文件仍由原桌面工具管理。

## 首次安装（Linux Docker Compose）

需要可用的 Docker Engine、Compose 插件，以及现有 HTTPS 反向代理。请先在测试环境验证并备份。以下只创建独立 Hub，不替换、停止或更新 Sub2API。

本仓库提供源码构建和 [正式镜像配置](../compose.release.yml) 两种替代方式，不能叠加或同时启动。正式 GHCR 镜像地址为 `ghcr.io/dkdjndbfj-wq/salcara-hub-standalone:版本号`，只使用已经出现在 `standalone-v版本号` Release 中的版本，缺少 Release 或镜像拉取失败时不能认为发布已完成。同一个容器已经包含轻量启动器，可以在管理页检查并一键更新 **Hub 应用程序**；不会替换 Docker 镜像、启动器或操作服务器上的其它容器。签名源不可达时原应用继续运行。不要把插件 Release 的 `update.json` 当作应用或镜像更新源。

正式镜像首次安装（Release 确认可用后）：

```sh
export SALCARA_HUB_PUBLIC_URL=https://你的中转站域名/salcara-hub
export SALCARA_HUB_IMAGE_VERSION=0.4.0
docker compose -f compose.release.yml config --quiet
docker compose -f compose.release.yml pull hub
# 新卷只执行一次。已安装的卷不能重新初始化。
docker compose -f compose.release.yml run --rm --no-deps hub -init
docker compose -f compose.release.yml up -d --no-deps hub
docker compose -f compose.release.yml ps
```

下面保留源码构建流程。使用正式配置时，后续每条 Compose 命令也必须带 `-f compose.release.yml`，避免误切回源码镜像。两份配置固定同一个项目名及卷名，使已有源码部署可保留 `hub-data` 转为正式镜像；转换前先备份、检查最终卷名称，不能执行 `down -v`。

```sh
git clone https://github.com/dkdjndbfj-wq/salcara-hub-plugin.git
cd salcara-hub-plugin
export SALCARA_HUB_PUBLIC_URL=https://你的中转站域名/salcara-hub
docker compose config --quiet
docker compose build hub
# 只运行一次：在独立数据卷生成新管理令牌，0600，不打印，不覆盖旧令牌。
docker compose run --rm --no-deps hub -init
docker compose up -d --no-deps hub
docker compose ps
```

请保留 `SALCARA_HUB_PUBLIC_URL` 的配置，日后在同一 Compose 项目目录更新。它必须为本站 HTTPS URL，路径为 `/salcara-hub`。公网访问不要直连 8787；Compose 仅绑定宿主机 `127.0.0.1:8787`，由 Nginx 暴露 HTTPS。

**只有一个独立 Hub 容器**，里面运行 Hub 与很小的 Go 启动器两个非 root 进程，共享一个资源硬限制，不是为了更新再运行第二个 Docker 服务。运行镜像只读；`/data` 卷保存绑定、封禁、审计、有期限的命令回执、资源模式和已验证更新程序。临时 `/tmp` tmpfs 只放内部 Unix socket 与新随机控制令牌，权限 0600，不通过公网暴露。无需另外运行数据库、Redis、Node 或 Docker 更新器服务，不挂 Docker socket。

如需在网页管理页输入新管理令牌，由你在自己的服务器上取回文件，不要发送给客服、AI 或放进 iframe URL：

```sh
# 目标文件必须是尚不存在的新文件；它是管理秘密，需要妥善保管。
docker compose cp hub:/data/admin-token ./salcara-hub-admin-token.txt
chmod 600 ./salcara-hub-admin-token.txt
```

在自己的安全编辑器中打开文件，将令牌粘贴到 Hub 管理页。页面只在内存中保留登录凭据，不保存到 localStorage、不设登录 Cookie；刷新后需要重新输入。“退出管理”会清除页面内存中的令牌。管理员接口无正确独立 Bearer 令牌时返回 401。原 Sub2API 登录令牌和模型 Key 都不能替代它。请对这份令牌及数据卷备份限制访问。

## Nginx 与自定义 iframe 菜单

将 [Nginx 路径片段](../deploy/standalone/nginx-location.conf) 中的两个 `location` 合并到现有 HTTPS `server {}`，先执行 `nginx -t`，成功后 reload。**不要覆盖整个原配置**；不要更改现有 `/v1/`、`/api/`、`/`、TLS 路由。片段假定 Nginx 位于宿主机；如果 Nginx 也是容器，需要仅给这两个服务配置独立私有 Docker 网络，将 upstream 改为 `http://hub:8787`，不能使用 Nginx 容器自己的 `127.0.0.1`，也不要公开 Hub 端口。

在 Sub2API 管理员的系统设置 → 自定义菜单（自定义 iframe 页面）新增：

| 名称 | URL（替换成自己的域名） | 可见范围 |
| --- | --- | --- |
| 远程管理 | `https://你的中转站域名/salcara-hub/admin/` | 管理员 |
| 远程使用说明 | `https://你的中转站域名/salcara-hub/` | 普通用户 |

普通用户页只展示使用说明及公开通道是否可达，不提供全站设备、用户数量、延迟统计或管理操作。管理员页展示注册/已配对/在线设备和电脑上报的会话状态、Hub ↔ 电脑往返测量、查询、封禁/解封、断开、解除配对和审计。上报任务状态不是对电脑进程的独立检查，测量也不是手机到服务器的 RTT。管理页不会展示用户聊天内容或 API Key。

**重要：官方 Sub2API v0.2.11 的自定义 iframe 会自动向 URL 追加本站登录 `token`、用户等参数。** [官方页面源码](https://github.com/Wei-Shaw/sub2api/blob/v0.2.11/frontend/src/views/user/CustomPageView.vue) 和 [URL 构造实现](https://github.com/Wei-Shaw/sub2api/blob/v0.2.11/frontend/src/utils/embedded-url.ts) 可核验此行为。

- 菜单只能指向你信任并控制的同源 HTTPS 页面，不要指向第三方服务；同源页面本身也是需要信任的代码。
- Hub 静态页收到 query 时会先返回 303 到无参数的同路径，不把 Sub2API 令牌当作登录凭据，不把参数传给 JS；页面设置 `Referrer-Policy: no-referrer`、`Cache-Control: no-store`。
- Nginx 的 Hub location 必须禁用 access log（片段已设置 `access_log off`），或自定义不含 `$request`、`$request_uri`、`$args` 的日志格式。也要检查 CDN/WAF、前置负载均衡和错误追踪是否记录完整 URL：后端的 303 **不能清除已经落入前置日志的 token**。
- 页面设置 `X-Frame-Options: SAMEORIGIN` 和 CSP `frame-ancestors 'self'`；不要给该路径追加冲突的 `DENY` / `frame-ancestors 'none'`。跨域 iframe 默认不支持。如仍被拦截，检查当前站点 CSP 的 `frame-src` 是否允许 `'self'`，只调整所需规则，不整体关掉 CSP。
- 隐藏“在新标签页打开”按钮不能代替日志保护和管理员鉴权。普通用户直接访问管理 URL 也不能获取管理 API 数据。

## 资源开销与省资源设计

Docker 不会让服务不占内存，也不是每个容器都需要运行一套虚拟机。容器是共享宿主内核的隔离进程，[Docker 官方解释](https://docs.docker.com/get-started/docker-concepts/the-basics/what-is-a-container/)。这套运行镜像只有两个 Go 二进制、CA 证书和许可证；编译用的 Go SDK 不在运行镜像中。它不会再启动一份 Sub2API。

默认 Compose 对整个 Hub 容器设置 **384 MiB 内存硬限制、1 个 CPU 配额、64 个进程上限**。父启动器的 `GOMEMLIMIT=32MiB`，不会与 Hub 一起各自拿到 320MiB；子 Hub 启动后根据保存的资源模式设置自己的 Go 软目标。软目标不是固定占用、RSS 上限或绝不 OOM 的承诺。

管理员控制台提供三种预设，默认“省资源”，切换需要确认并保存到 `/data/resource-mode`，重启后保留：

| 模式 | Hub Go 软内存目标 | 主要作用 |
| --- | --- | --- |
| 省资源 | 96 MiB | 较小事件暂存、消息队列和新连接/指令并发 |
| 均衡 | 192 MiB | 增加暂存与并发，兼顾资源和响应 |
| 高负载 | 320 MiB | 更多暂存和新连接/指令并发，需要服务器余量 |

模式调整真实缓存字节/数量与新请求准入限制，不是只修改页面标签；不会取消正在执行的电脑任务，也不改变用户资格、审批要求或 API Key。缩小缓存可能出现进度缓存缺口；手机下次同步会尝试向在线且已授权的电脑重读最近可读取内容，**不保证全部历史补齐**，离线或授权过期时可能无法补齐。不会删除电脑聊天、项目文件或配对信息。**页面切换不会改变 Docker 的 384MiB 硬限制**，也不承诺同时支持多少用户。提高模式前要检查整机余量；接近硬限仍可能发生 OOM、连接中断或容器重启。数值是初始保护配额而非实测占用，应根据设备数、文本大小和压测调整，不关闭 OOM 防护。

任务发送仍直接转发到在线电脑，不是先把完整会话存云端再执行。最近事件仅在内存中按条数、字节和全站预算限量保留，容量不足会清较旧内容，Hub 重启也会清空；目前不是每条事件固定时间到期的 TTL。命令回执另有 24 小时去重期限，两者不是永久聊天仓库或无限离线任务队列。电脑离线发送会失败（例如409），不能保证服务器替你一直排队。

手机默认按需读取/发送，不常驻保持管理面板后台轮询或手机 SSE。电脑仍需轻量长连接承接 NAT 后的任务，当前默认 20 秒 keep-alive；这不是全链路完全断开时手机也能实时唤醒电脑的保证。可配置 `SALCARA_HUB_PING_INTERVAL`（5s–1m），但必须一并匹配反代超时；不要让 keep-alive 比代理读超时还长。管理列表由用户手动刷新，测量只在点击时发生。

部署后用 `docker stats --no-stream` 看实际容器开销，用 `docker compose logs --tail 100 hub` 排查故障（不要转发含私人数据的日志）。TLS/CDN/反代、Docker daemon 的额外资源也需要纳入整台服务器容量。首次在小服务器源码编译会消耗比运行更多的内存、CPU 和磁盘；今后有经过验证的预构建镜像，服务器只需拉取镜像，省掉编译开销。参考 [Docker 官方资源约束说明](https://docs.docker.com/engine/containers/resource_constraints/)。

## 网页一键更新 Hub 应用：不更新 Sub2API 或镜像

管理员进入“远程管理” → “检查更新”，看到可用的签名版本后确认“一键更新”。页面不会后台自动检查、自动升级或重试提交；HTTP 202 只代表受理，**不是已经完成**。应用切换期间可能短暂断开，恢复后点击“查询更新进度”查看实际运行版本和结果。私有控制回执会先 flush 才开始更新，但公网断网时仍不能保证手机已收到回执，因此不能把没有回执当作“没执行”然后自动重发。

同容器启动器按以下顺序处理：

1. 重新获取固定更新源，核对你确认的版本和 SHA-256，验证固定发布者 Ed25519 签名、产品 `salcara-hub-standalone`、启动协议 1、数据格式 1；个人版或插件清单即使由同一个发布者签名也拒绝。单更新源最多 64KiB，单二进制最多 64MiB。
2. 下载对应 Linux 架构的 Hub 二进制，逐字节核验大小和哈希，保存原始签名清单。**全部校验完成前不停止现有 Hub。** 下载流式写盘，不把 64MiB 整包当作常驻内存缓存。
3. 优雅停止子 Hub，确认旧进程退出后，执行已打开且再次验证的程序文件；不执行 shell、不接受任意路径、Docker 命令或镜像地址。
4. 核验新进程自己的 PID、产品、版本和健康响应。通过后原子提交确认记录；健康失败时先确认失败进程停止，再启动原已验证程序。无法确认停止则拒绝启动第二个数据写入者。

数据卷与独立管理令牌保留，启动器自身、Docker 镜像与 Sub2API 不变；只重启 Hub 应用会短暂中断远程连接与在途请求。自动回退指**程序**，不是数据库或用户数据回滚。当前仅接受 `launcher_protocol=1`、`data_schema=1`，拒绝不同数据格式的新版本；发布者必须遵守同数据格式兼容约定。有迁移需求时须完整备份并走相应镜像升级/迁移流程。

当前默认源为 `https://raw.githubusercontent.com/dkdjndbfj-wq/salcara-hub-plugin/main/updates/standalone.json`，发布前不放假清单。它和插件更新源独立；服务器只信任编译时固定的 Salcara 公钥，不从 feed 自动添加信任、不把菜单/管理请求中的 URL 当更新源、不读取签名私钥。运营者可在容器环境设置 `SALCARA_HUB_UPDATE_FEED_URL` 指向可信的公网 HTTPS 443 镜像源，但签名身份仍固定。每次连接验证全部 DNS 地址并固定实际目标 IP，拒绝内网、非 HTTPS、其它端口和带登录信息的地址；最多五次重定向且不转发凭据。`SALCARA_HUB_DISABLE_UPDATES=true` 可关闭应用更新。

`/data/updates/state.json` 只保存签名清单引用，不保存任意执行路径。恢复时重新验签并校验当前/前一确认程序的哈希；状态或程序损坏会拒绝启动而不是绕过验证，需运营者检查数据卷并使用完整备份恢复。更新目录保留确认程序与旧版本，历史缓存会随版本增加而占磁盘，**当前没有自动清理磁盘版本的承诺**。不要盲删 `/data/updates`、`state.json` 或锁文件；保存状态/目录同步失败可能导致持久记录不确定，页面会提示人工检查，不能当成可靠回退已完成。

## 什么时候还需要 Docker 命令更新？

Hub 和 Sub2API 可以分别升级。网页更新 Hub 程序不重建容器；要更新底层镜像、启动器或容器配置，仍需拉取/构建新的镜像并替换 Hub 容器，**不重启 Sub2API**。更新官方 Sub2API 不需要重新打 Hub 宿主补丁。不过仍要验收 HTTPS 路由、iframe 行为和桌面/手机客户端协议，不承诺未来所有版本无条件兼容。

网页不能一键替换 Docker 镜像或启动器。进行镜像更新时，先查看目标版本的发布说明、兼容范围与数据迁移要求，不默认自动追踪未知 `latest`。进入原 Compose 项目目录，在低峰执行：

```sh
# 先在新目录备份原 Hub 持久卷和部署配置，并记录当前源码 commit / 镜像 ID。
docker compose stop hub
# ./hub-backup-before-update 必须是新的、受访问控制的备份路径。
docker compose cp hub:/data ./hub-backup-before-update
chmod -R go-rwx ./hub-backup-before-update
docker compose start hub
docker image tag salcara-hub-standalone:local salcara-hub-standalone:before-update
git fetch origin --tags
# 按已经发布并核验的目标 tag 或 commit 切换代码；不得强行覆盖本地配置修改。
# 无本地修改且确认升级分支时，也可使用 git pull --ff-only。
docker compose build hub
docker compose up -d --no-deps hub
docker compose ps
```

更新前后的 `/data` 卷与管理令牌不变，不要再运行 `-init`，不要执行 `docker compose down -v` 或删除卷。备份可能包含管理令牌、设备秘密和转发回执，需要限制权限并考虑加密。已有已确认签名应用时，替换镜像会保留数据卷里的该应用选择，**不是强制切回镜像内的初始程序**；实际 Hub 版本以管理页为准。上面只有一个 Hub 实例，容器替换仍会短暂中断远程连接/在途转发；原桌面任务通常在电脑继续运行，但不能承诺所有在途远程请求都不会失败。手机重连和命令去重有期限，不是永久 exactly-once。

容器不健康时，先保留新容器日志和数据备份，在确认**没有不兼容数据迁移**后可恢复旧镜像：

```sh
docker image tag salcara-hub-standalone:before-update salcara-hub-standalone:local
docker compose up -d --no-deps --no-build hub
```

有数据迁移则需要按照目标版本的回滚说明恢复备份，不能仅靠改镜像保证可回退。没有备份时不要猜测性回滚。此独立数据卷与 Sub2API 的数据库备份是两回事。

这里刻意不挂 `/var/run/docker.sock`，不接收 shell、任意镜像 URL 或任意 Compose 文件。网页应用更新已由同一容器内的非 root 启动器处理；它没有控制宿主 Docker daemon 的权限，也无需为此再增加一个服务。若以后还希望“网页更新镜像”，需要另外设计明确启用的、仅能操作固定 Hub 项目的宿主受限更新机制，不能直接暴露通用 Docker API。Docker Compose 的基础镜像更新流程见 [官方部署说明](https://docs.docker.com/compose/how-tos/production/)。

## 验证范围

[独立 Docker CI](../.github/workflows/standalone-docker.yml) 只使用 `contents: read`，跑 UI 与 Go/竞态测试，构建 Linux amd64、运行临时非 root/只读单容器的 smoke test，然后交叉构建 arm64；不登录镜像仓库、不推送镜像、不发布 APK、不部署生产。临时 smoke test 验证独立随机 token、安全初始化、健康、前缀隔离、iframe 响应头、管理员鉴权、三种资源模式及确认/持久化、同容器应用更新状态不主动联网，并只清理自己标记的新建容器与卷。它会记录无真实用户时的临时容器 `docker stats`，这不是生产容量或有负载的资源保证。

CI 成功后提供 `salcara-hub-standalone-development` 测试 artifact（14天）：amd64 Docker archive、arm64 OCI archive、两架构 Hub/启动器二进制及说明。它们是未签名的开发产物，不是正式应用更新源；不要绕过更新器把这些文件放进 `/data/updates`。

Docker CI 成功并不等于目标服务器反代/TLS、真实手机扫码及 Codex / Claude Desktop 续聊验收成功；arm64 交叉编译也不是 arm64 实机运行测试。本地 Docker daemon 未启用时，不会宣称已经在本地构建并运行了容器。构建器使用 [Docker 官方 Go 镜像](https://hub.docker.com/_/golang) 的 `golang:1.27.1-bookworm`，运行阶段为 `scratch`。

## 安全加固（v1.5 Hub）

- **失败限流按真实客户端计算**：Compose 默认 `SALCARA_HUB_TRUST_PROXY=true`，并只在 `127.0.0.1:8787` 发布端口；Nginx 必须用 `proxy_set_header X-Forwarded-For $remote_addr;`（覆盖，不是追加）。旧版配置清空了这个头，所有访客在 Hub 看来都是同一个地址：任何人连续猜错 20 次，全站手机和电脑都会被 429 拒绝一分钟。若站点在 Cloudflare 等 CDN 后面，先配置 `ngx_http_realip_module`。
- **有效凭据不受他人失败影响**：已配对手机的令牌、已登记电脑的设备密钥先校验，校验通过就放行；只有失败的请求才计入和受限。
- **IPv6 按 /64 计数**：同一台主机换地址不能绕过限流；失败记录表满时不会再“全部拒绝”。
- **可选的 Nginx 外层限流**（在 `http {}` 里，按需调整）：

```nginx
limit_req_zone $binary_remote_addr zone=salcara_hub:10m rate=20r/s;
# 然后在 location ^~ /salcara-hub/ 里加：
limit_req zone=salcara_hub burst=60 nodelay;
```

- **长轮询**：Hub 1.5 支持 `events.wait.v1`（`/app/events?wait=1..25`），手机看对话时由服务器挂起空请求、有新进展立即返回；`proxy_read_timeout 90s` 已足够。
