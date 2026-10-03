# 中转站独立 Docker：发布与更新维护

与个人版、原插件分开：产品 `salcara-hub-standalone`，镜像 `ghcr.io/dkdjndbfj-wq/salcara-hub-standalone:0.4.0`，Git 标签 `standalone-v0.4.0`，清单 `updates/standalone.json`。原插件 `v0.3.1` 和 `update.json` 不改，不发布个人版。

## 首次及以后发布

1. 完成源码审查，在准备发布的固定 commit 运行 `Standalone release build`，输入裸版本号，例如 `0.4.0`；默认 `publish_image=false` 只构建验证，不部署服务器。CI 对 amd64 跑实际非 root / 只读容器测试，并交叉构建 arm64；arm64 不属于实机验收。
2. 验证 CI 成功以及提交 SHA 后，下载对应 `standalone-release-版本号` artifact。它包含两架构 Hub / launcher、`SHA256SUMS` 和未签名的 `standalone-payload.json`。检查实际二进制大小 / 哈希与 payload 一致。不能把未签名 payload 放进更新源。
3. 在同一源码 commit 用仓库外、既有 `salcara-local-20260930` Ed25519 私钥签名；不换发布者身份，不把私钥上传 GitHub 或服务器。下面示例只表示路径，不能把私钥内容写进命令或日志：

```sh
cd plugins/salcara-hub
go run ./tools/standalone-feed \
  -payload /安全的新目录/standalone-payload.json \
  -public-identity ../../publisher/public.json \
  -private-key /仓库外私有目录/publisher.private.b64 \
  -output /安全的新目录/standalone.json
```

4. `publish_image=true` 在同一 commit 发布 GHCR 固定版本镜像；只使用 Actions 自带的 `GITHUB_TOKEN`（`packages:write`），不需要私钥 Secret。不使用未知 `latest`，不覆盖已发布的版本号。确认镜像为公开可拉取，并记录 multiarch digest。
5. 给同一 commit 创建新的 `standalone-v版本号` 标签和 GitHub Release，上传 `salcara-hub_版本号_linux_amd64`、`salcara-hub_版本号_linux_arm64`、两架构 launcher、`SHA256SUMS`、签名后的 `standalone.json` 及说明。先确认两个 Hub HTTPS 下载均可达，并核对下载后的实际哈希。
6. 最后把同一已验证签名清单发布到 `main/updates/standalone.json`。这是唯一会改变管理页检查结果的步骤，必须在 release asset 可下载之后做。不修改原插件更新源，也不自动发布个人版。

`tools/standalone-feed` 只读取一个明确传入的外部私钥、校验它与公开身份一致、生成新的 public envelope；不联网、不上传、不覆盖已存在的输出。必须备份签名私钥，否则旧安装将不能接受未来由新身份签名的版本。

签名后、上传前从仓库根目录独立验证（只读公开文件）：

```sh
node deploy/standalone/verify-release.mjs \
  /安全的新目录 0.4.0 /安全的新目录/standalone.json publisher/public.json
```

它必须显示固定发布者、签名、产品、版本及两份实际 Hub 文件一致。发布后下载实际 HTTPS assets 到新的空目录，再用同一命令复验，不能只检查上传动作成功。

## 管理页更新行为

现有「检查更新 / 一键更新 / 查询更新进度」界面不变。默认固定的 HTTPS 源和公钥已经启用；发布清单前或网络不可达时显示不可用，旧服务继续运行。不会未经管理员确认静默升级。

接受 202 后后台下载、验签、核对大小 / 哈希、确认旧写入进程已退出，再执行已验证 inode。新进程 PID / 产品 / 版本 / 健康一致才提交；失败时恢复旧程序。管理令牌、配对、封禁、清理设置、项目及电脑会话不因程序更新清空。回退是程序回退，不是用户数据回滚。

网页更新只替换 **Hub 子程序**；launcher、CA、镜像、Compose 等仍通过 Docker 版本更新。不能把网页按钮宣传成全部 Docker 底层自动更新，不能给它 Docker socket 权限。

## 正式镜像升级

先备份持久卷与部署配置；确认不存在不兼容的数据迁移后，保持同一项目名和卷：

```sh
export SALCARA_HUB_IMAGE_VERSION=已发布并确认的新版本
docker compose -f compose.release.yml pull hub
docker compose -f compose.release.yml up -d --no-deps hub
docker compose -f compose.release.yml ps
```

不能再执行 `-init` 或 `down -v`。切换镜像不会强行降级数据卷中已经确认的签名程序。不能同时运行源码 Compose、正式 Compose、原插件或个人版去写同一数据卷。
