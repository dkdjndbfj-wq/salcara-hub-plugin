# Salcara Hub：发布与更新维护

产品标识 `salcara-hub-standalone`，本轮正式发布目标为 **0.5.7**；目标镜像 `ghcr.io/dkdjndbfj-wq/salcara-hub-standalone:0.5.7`，目标 Git 标签 `standalone-v0.5.7`，新认证版本更新清单 `updates/standalone-account.json`。本文中的版本号是发布准备配置，安装和升级命令须在正式 Release、公开可拉取镜像及实际下载校验全部确认后执行。发布时保留兼容标识和已发布版本的公开下载地址；新版本替换当前更新指针，但不重签或覆盖旧版本资产、旧协议更新源。

本次配套版本为 **Android 1.8.7（versionCode 28）/ Desktop（Bridge）1.6.10**。Hub 0.5.7 仅同步公开首页和状态页的配套版本清单，并更新本版构建、安装目标与文档；不改 Hub 业务、身份、配置或最低准入策略，沿用既有发布签名身份。客户端修复由对应客户端提供，不是 Hub 0.5.7 新增的业务修复；本轮范围见 [0.5.7 版本说明](RELEASE-0.5.7.md)，0.5.6 既有修复保留在 [历史说明](RELEASE-0.5.6.md)。本轮先准备正式构建、独立产物验证和 Release 草稿；三端统一确认后才公开，实际公开下载验签成功后才推进账号更新源。正式公开与线上部署须分别记录；不能从候选源码或草稿推定完成，配套版本页只有在对应 Hub 构建实际运行后才会更新。

0.5.0 首次部署自动生成每站独立的管理密钥，没有账号。0.4.0 的管理令牌不再用于管理页登录。旧安装不能直接通过网页跨过认证迁移：先按 [部署指南](STANDALONE-DOCKER.zh.md) 停机备份、生成新认证文件，再切换镜像；配对及设备数据保留。旧更新源停留在 0.4.0，新安装只使用新更新源。

0.5.3 引入管理员账号，旧 0.5.0–0.5.2 安装首次升级后以 `admin` 和原管理密钥登录，不重新初始化；之后可修改账号和密码，已有自定义账号和密码的安装继续沿用。首次启用后台自动检测还需更新含新版 launcher 的完整镜像，网页更新只替换 Hub 子程序。

## 配套客户端版本同步

每次更新或发布 Hub、手机端、Bridge 时，都要核对 `internal/standalone/web/client-versions.json` 中的配套版本。公开首页和连接状态页共用这份配置；版本值应与本次确认的手机端、Bridge 发布源码一致，不在 HTML 中另写一套版本号。配套版本展示与 `SALCARA_HUB_DESKTOP_MINIMUM_VERSION` 最低准入策略相互独立，不能因更新展示值而自动提高最低版本。

维护时使用个人技能 `salcara-release-version-sync`，运行其中的版本检查脚本核对源码及实际页面，并运行 `go test ./internal/standalone -run TestPublicClientVersions -count=1`。同时检查首页、状态页在亮色、暗色和手机宽度下的版本展示。

这份配置会嵌入 Hub 二进制。修改配置或单独发布客户端不会更新已经部署的页面；需要重新构建并按既有流程更新 Hub，随后对实际站点再次校验。发布记录应区分本地已修改、已构建和已上线。

## 首次及以后发布

1. 完成源码审查，在准备发布的固定 commit 运行 `Standalone release build`，输入裸版本号，例如 `0.5.7`；默认 `publish_image=false` 只构建验证，不部署服务器。一次构建导出包含 provenance / SBOM 的双架构 `image.tar` OCI 归档。构建阶段设置正式镜像名称以生成非空的证明 subject，但不上传镜像。CI 校验整个引用图的大小 / SHA-256、平台、版本及每架构证明的 subject 绑定，再从该归档导入 amd64，运行实际非 root / 只读容器测试；从已导入镜像再次 `docker image save` 得到的 config 原始字节摘要必须与 OCI config digest 一致。不把 `docker inspect.Id` 一律当作 config digest：新版 containerd 镜像存储可能返回 manifest digest。arm64 只导入、检查并从未启动的容器导出二进制，不属于实机验收。
2. 验证 CI 成功以及提交 SHA 后，下载对应 `standalone-release-版本号` artifact。它包含同一 OCI 归档、`image-receipt.json`、从该镜像导出的两架构 Hub / launcher、`SHA256SUMS` 和未签名的 `standalone-payload.json`。receipt 记录源码 SHA、归档哈希 / 大小、multiarch index digest 和两架构 config / manifest digest。检查实际二进制大小 / 哈希与 payload 一致。不能把未签名 payload 放进更新源。
3. 在同一源码 commit 用仓库外、既有 `salcara-local-20260930` Ed25519 私钥签名；不换发布者身份，不把私钥上传 GitHub 或服务器。下面示例只表示路径，不能把私钥内容写进命令或日志：

```sh
# 从仓库根目录执行
go run ./tools/standalone-feed \
  -payload /安全的新目录/standalone-payload.json \
  -public-identity publisher/public.json \
  -private-key /仓库外私有目录/publisher.private.b64 \
  -output /安全的新目录/standalone-account.json
```

4. 需要发布镜像时，该次运行选择 `publish_image=true`；发布 job 只下载本次已成功构建 / smoke 的精确 artifact ID，不重新构建。它先用 build job 输出的哈希验证归档，再复验 OCI 图 / receipt，用 `skopeo --all --preserve-digests` 保留两个平台及 provenance / SBOM，复制到固定 GHCR 版本标签，最后核对实际返回的 index 字节与 digest。只使用 Actions 自带的 `GITHUB_TOKEN`（`packages:write`），不需要私钥 Secret。若先做一次不发布的试跑、再发起发布运行，后者是另一份产物，必须采用后者的 receipt / 测试记录，不能混用前一次记录。确认镜像为公开可拉取，并记录 multiarch digest。
5. 给同一 commit 创建新的 `standalone-v版本号` 标签和 GitHub Release，上传 `salcara-hub_版本号_linux_amd64`、`salcara-hub_版本号_linux_arm64`、两架构 launcher、`SHA256SUMS`、签名后的 `standalone-account.json` 及说明。先确认两个 Hub HTTPS 下载均可达，并核对下载后的实际哈希。
6. 最后把同一已验证签名清单发布到 `main/updates/standalone-account.json`。这是唯一会改变新认证版本管理页检查结果的步骤，必须在 release asset 可下载之后做。不要覆盖旧 `updates/standalone.json`，否则旧版本可能收到缺少认证初始化文件的跨版本更新。

`tools/standalone-feed` 只读取一个明确传入的外部私钥、校验它与公开身份一致、生成新的 public envelope；不联网、不上传、不覆盖已存在的输出。必须备份签名私钥，否则旧安装将不能接受未来由新身份签名的版本。

### 镜像防覆盖与失败处理

本仓库工作流按 `repository + 裸版本号` 跨分支排队，不取消运行中的发布。写入前使用认证的 GHCR 查询；只有结构明确的 `404 MANIFEST_UNKNOWN / NAME_UNKNOWN` 才允许首次发布。已有版本、鉴权失败、限流、网络错误、HTML 404 或无法判断的响应全部拒绝，不以“查询失败”当成“版本不存在”。

该保护约束遵守此工作流的发布者，**不是注册表级全球不可变或原子 compare-and-set**。仓库外其他有写权限的账号仍可能在查询和写入之间修改标签；必须限制正式镜像命名空间的写权限，不让其他工作流 / 手工操作覆盖正式版本。不要使用 `latest`、`--clobber` 或删除旧标签再重发同版本。

如果上传中途断线、超时或最终 digest 核验失败，流程不会自动重试覆盖。先查询固定版本标签并与该次 `image-receipt.json` 对照：未能确认前不发布应用更新指针；已存在的版本由管理员调查，不绕过拒绝检查重新推送。真实 Docker / skopeo 工具链或未来 OCI 格式不符合校验时同样停止，不能跳过证明检查。

### 可复现验收与剩余门槛

部署和发布工具的合成回归（不联网、不使用真实令牌或执行生产容器）。两个工作流都运行全部现有 Node 测试，包含公开首页、服务授权状态、用户权限、来源和入口管理，避免只校验管理页：

```sh
bash deploy/standalone/install.test.sh
bash deploy/standalone/bot-deploy.test.sh
node --test deploy/standalone/*.test.mjs
```

功能测试实际构造 OCI blob 图，覆盖损坏哈希 / 错误平台 / 错误版本 / 缺少证明 / 错误或缺失 subject，以及 Docker config 替换、归档路径穿越 / 重复项、认证 404、已存在、限流、网络失败、上传不确定及发布 digest 不同的拒绝分支；工作流契约另验证跨 ref 排队、构建镜像命名和发布 job 不重建。合成通过不等于真实工具链或 GHCR 已验收。

还可显式运行真实工具的离线传输回归：先自行拉取官方 `quay.io/skopeo/stable`，再运行 `node deploy/standalone/release-image-tools-smoke.mjs quay.io/skopeo/stable:latest --buildkit-fixture`（可把标签换成已核对的 `@sha256:...`）。它只把新建合成目录挂入断网、只读、限额工具容器，不挂 Docker socket、不使用账号令牌；核对 OCI 归档往返的 index / provenance / SBOM 未改变，并把合成的两架构 Docker 归档加载后重新保存、核验原 config 字节。可选 `--buildkit-fixture` 另构建一个只有文本的 scratch 镜像，不包含 RUN、外部基础镜像或真实 Hub 数据，验证真实 BuildKit provenance subject 确实绑定目标 runtime manifest。它不会启动合成镜像，完成后清理本次带专用随机名称的容器、镜像标签及临时目录。

2026-10-14 已在 Windows Docker Desktop / Linux engine 完成上述隔离工具回归：skopeo 1.22.3、BuildKit v0.32.2，OCI 往返 / 两架构导入后的 config 字节 / 实际 provenance v1 subject 绑定均通过。真实工具使用 `docker-archive` 加载路线以避免挂载 Docker socket；Linux CI 的 `docker-daemon` 路线、正式 Hub 双架构构建的实际 SBOM 和最终 GHCR 上传仍需同源码 CI 验收。该记录不是正式镜像或 arm64 运行验收。

在隔离 Linux 环境安装 Docker / Buildx / skopeo 后，可对下载的归档离线复验。以下值必须取自同一成功 build job 的输出和源码 SHA，不使用另一次运行的 receipt 代替：

```sh
node deploy/standalone/release-image.mjs check-archive /新空目录/image.tar sha256:该次构建归档摘要
skopeo copy --all --preserve-digests oci-archive:/新空目录/image.tar oci:/另一个新空目录/oci:release
node deploy/standalone/release-image.mjs verify /另一个新空目录/oci /新空目录/image.tar /新空目录/image-receipt.json 版本号 40位源码SHA sha256:构建index摘要 sha256:构建归档摘要
```

正式发布前仍需该源码 SHA 的完整 Linux CI 成功，包含真实 skopeo 导入 / config 原始字节对照 / 非 root 容器 smoke / 最终归档复验；arm64 实机测试、公开拉取及实际 GHCR digest 核验必须单独记录。离线应用清单签名与 HTTPS 资产复验仍按前述流程进行，OCI provenance / SBOM 不能代替既有 Ed25519 应用更新签名。

签名后、上传前从仓库根目录独立验证（只读公开文件）：

```sh
node deploy/standalone/verify-release.mjs \
  /安全的新目录 0.5.7 /安全的新目录/standalone-account.json publisher/public.json
```

它必须显示固定发布者、签名、产品、版本及两份实际 Hub 文件一致。发布后下载实际 HTTPS assets 到新的空目录，再用同一命令复验，不能只检查上传动作成功。

## 管理页更新行为

现有「检查更新 / 一键更新 / 查询更新进度」界面不变。默认固定的 HTTPS 源和公钥已经启用；发布清单前或网络不可达时显示不可用，旧服务继续运行。不会未经管理员确认静默升级。

接受 202 后后台下载、验签、核对大小 / 哈希、确认旧写入进程已退出，再执行已验证 inode。新进程 PID / 产品 / 版本 / 健康一致才提交；失败时恢复旧程序。管理密钥、配对、封禁、清理设置、项目及电脑会话不因程序更新清空；管理员登录会话在 Hub 进程重启后失效，需要重新登录。回退是程序回退，不是用户数据回滚。

网页更新只替换 **Hub 子程序**；launcher、CA、镜像、Compose 等仍通过 Docker 版本更新。不能把网页按钮宣传成全部 Docker 底层自动更新，不能给它 Docker socket 权限。

## 正式镜像升级

先备份持久卷与部署配置；确认不存在不兼容的数据迁移后，保持同一项目名和卷：

```sh
export SALCARA_HUB_IMAGE_VERSION=已发布并确认的新版本
docker compose -f compose.release.yml pull hub
docker compose -f compose.release.yml up -d --no-deps hub
docker compose -f compose.release.yml ps
```

不能再执行 `-init` 或 `down -v`。切换镜像时，启动器先验签并核对现有缓存：镜像内置版本更高才尝试它，健康 / 产品 / PID / 版本通过后才提交；旧签名缓存作为回退程序保留。镜像不高于缓存则继续使用缓存，不降级。若新镜像内置程序不健康则恢复旧缓存，并记住失败镜像的版本与哈希；普通重启不会反复尝试同一个坏镜像，更换不同版本或内容的镜像后才允许重试。任何路径或签名校验错误仍拒绝执行，不借镜像升级绕过缓存校验。

镜像升级不等于数据回滚；不要手工换成不兼容的旧镜像。0.5.5 的 Bot 手机撤销和重置使用 `bot-phone-intents.json` 与主状态共同恢复，完整备份必须包含这些状态。存在待完成撤销/重置时，不得让不理解该日志的旧版本直接接管数据，也不得删除日志或回放旧备份来“解锁”。回退前先保留现场、核对数据兼容性和最新撤销记录；旧备份可能恢复已撤销的权限。不能同时运行两套服务去写同一数据卷，包括源码 Compose 和正式 Compose。

## 更新缓存与磁盘

`/data/updates` 保留历史已验证二进制，当前不自动清理，也没有总容量上限。单份 Hub 更新二进制最多 64 MiB，但长期多次升级的磁盘占用会累加；应监控持久卷可用空间并纳入备份策略。下载、校验或更新状态写入失败时不会把未确认版本当成功；新版本状态提交失败会停止未提交的程序。不要手工删除当前 / 回退程序或 `state.json`，否则启动会按安全边界拒绝继续。此限制不应宣传成已具备自动缓存维护。
