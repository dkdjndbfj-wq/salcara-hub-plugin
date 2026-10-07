# Salcara Hub：发布与更新维护

产品标识 `salcara-hub-standalone`，当前镜像 `ghcr.io/dkdjndbfj-wq/salcara-hub-standalone:0.5.2`，Git 标签 `standalone-v0.5.2`，新认证版本更新清单 `updates/standalone-account.json`。发布时保留兼容标识和已发布版本的公开下载地址；新版本替换当前更新指针，但不重签或覆盖旧版本资产、旧协议更新源。

0.5.0 首次部署自动生成每站独立的管理密钥，没有账号。0.4.0 的管理令牌不再用于管理页登录。旧安装不能直接通过网页跨过认证迁移：先按 [部署指南](STANDALONE-DOCKER.zh.md) 停机备份、生成新认证文件，再切换镜像；配对及设备数据保留。旧更新源停留在 0.4.0，新安装只使用新更新源。

## 首次及以后发布

1. 完成源码审查，在准备发布的固定 commit 运行 `Standalone release build`，输入裸版本号，例如 `0.5.2`；默认 `publish_image=false` 只构建验证，不部署服务器。CI 对 amd64 跑实际非 root / 只读容器测试，并交叉构建 arm64；arm64 不属于实机验收。
2. 验证 CI 成功以及提交 SHA 后，下载对应 `standalone-release-版本号` artifact。它包含两架构 Hub / launcher、`SHA256SUMS` 和未签名的 `standalone-payload.json`。检查实际二进制大小 / 哈希与 payload 一致。不能把未签名 payload 放进更新源。
3. 在同一源码 commit 用仓库外、既有 `salcara-local-20260930` Ed25519 私钥签名；不换发布者身份，不把私钥上传 GitHub 或服务器。下面示例只表示路径，不能把私钥内容写进命令或日志：

```sh
# 从仓库根目录执行
go run ./tools/standalone-feed \
  -payload /安全的新目录/standalone-payload.json \
  -public-identity publisher/public.json \
  -private-key /仓库外私有目录/publisher.private.b64 \
  -output /安全的新目录/standalone-account.json
```

4. `publish_image=true` 在同一 commit 发布 GHCR 固定版本镜像；只使用 Actions 自带的 `GITHUB_TOKEN`（`packages:write`），不需要私钥 Secret。不使用未知 `latest`，不覆盖已发布的版本号。确认镜像为公开可拉取，并记录 multiarch digest。
5. 给同一 commit 创建新的 `standalone-v版本号` 标签和 GitHub Release，上传 `salcara-hub_版本号_linux_amd64`、`salcara-hub_版本号_linux_arm64`、两架构 launcher、`SHA256SUMS`、签名后的 `standalone-account.json` 及说明。先确认两个 Hub HTTPS 下载均可达，并核对下载后的实际哈希。
6. 最后把同一已验证签名清单发布到 `main/updates/standalone-account.json`。这是唯一会改变新认证版本管理页检查结果的步骤，必须在 release asset 可下载之后做。不要覆盖旧 `updates/standalone.json`，否则旧版本可能收到缺少认证初始化文件的跨版本更新。

`tools/standalone-feed` 只读取一个明确传入的外部私钥、校验它与公开身份一致、生成新的 public envelope；不联网、不上传、不覆盖已存在的输出。必须备份签名私钥，否则旧安装将不能接受未来由新身份签名的版本。

签名后、上传前从仓库根目录独立验证（只读公开文件）：

```sh
node deploy/standalone/verify-release.mjs \
  /安全的新目录 0.5.2 /安全的新目录/standalone-account.json publisher/public.json
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

镜像升级不等于数据回滚；不要手工换成不兼容的旧镜像。不能同时运行两套服务去写同一数据卷，包括源码 Compose 和正式 Compose。

## 更新缓存与磁盘

`/data/updates` 保留历史已验证二进制，当前不自动清理，也没有总容量上限。单份 Hub 更新二进制最多 64 MiB，但长期多次升级的磁盘占用会累加；应监控持久卷可用空间并纳入备份策略。下载、校验或更新状态写入失败时不会把未确认版本当成功；新版本状态提交失败会停止未提交的程序。不要手工删除当前 / 回退程序或 `state.json`，否则启动会按安全边界拒绝继续。此限制不应宣传成已具备自动缓存维护。
