# 2026-10-04 中转站 Docker 发布及自动更新交接

## 本次范围与基线

- 工作目录：`work/salcara-hub-release-20261004`，独立复制，不修改 Claude 工作目录或旧 `salcara-hub-plugin-github` dirty checkout。
- Git 基线：旧公开仓库 commit `7ce737f`。Hub 最新 `cmd/`、`internal/` 及源码 `compose.yml` 取自 `salcara-phone-dialog-review/output/docker-kernel-review`，包含 Claude 已完成的设备容量、自动清理和 FCM 推送改动；不重新设计或修改它们。
- 不发布、不修改个人版。原插件已发布 `v0.3.1`、插件 ID、宿主补丁和插件更新源保留。本次独立 Docker tag 为 `standalone-v0.4.0`。
- 手机、桌面界面及动效不在本次 Docker工作范围。

## 自动更新补全

1. **产品绑定**：`internal/launcher/{config,feed}.go` 在签名 payload 中要求 `product:"salcara-hub-standalone"`。即使公钥相同，个人版、插件或缺失产品的 feed 仍被拒绝；增加 `product_test.go` 回归。此源之前未正式发布，不兼容手工生成的不含 product 的私人旧 feed。
2. **执行后检查**：`internal/launcher/process_linux.go` 与 `internal/standalone/{config,server}.go` 添加并要求健康响应的精确 product；原 PID、版本和健康验证继续保留。新增 Linux 子进程测试验证产品错误时先停止错误子进程再允许旧版恢复。
3. **签名工具一致性**：`tools/standalone-feed/{main,main_test}.go` 要求六个精确公开字段及 standalone product；原外部私钥边界、身份匹配、签名、大小、哈希、版本、URL 和不覆盖输出约束保留。未查看、打印、提交或上传真实私钥。
4. **版本一致性**：Dockerfile 增加 VERSION 构建参数，同时写入 Hub、launcher 的编译版本及 OCI version label；否则正式 0.4.0 清单和实际 `0.4.0-dev` 健康响应会不一致。把 builder race 覆盖扩大为 `./...`，包括 Claude 最新 Hub。
5. **可发布链路**：新增 `standalone-release.yml`，默认只构建验证。amd64 真实容器 smoke 通过后导出两架构原始二进制；`publish_image=true` 才推固定版本 GHCR，不推 `latest`。CI 无私钥 Secret、无生产部署、无原插件/个人版 release。
6. **公开产物准备与独立验证**：`release-stage.mjs` 验证 Linux ELF64 及机器架构，生成真实大小、SHA-256、standalone tag 的 URL 与未签名 payload；`verify-release.mjs` 只读公钥、签名和二进制，核验固定发布者及产品/版本/大小/哈希。不得把未签名 payload 当更新源。
7. **部署配置**：新增独立替代的 `compose.release.yml`，固定正式镜像版本，保留现有项目及 `hub-data` 卷。不能同时使用源码/正式 Compose 启动两个写入者。`-init` 仅新卷一次；升级不清空数据、不用 `down -v`。

## 保留的界面和安全链路

对 Claude 最新 `cmd/`、`internal/` 作规范化换行比较：62 个基线文件中 55 个未变，只有 7 个自动更新产品绑定相关文件改动（其中 2 个是测试）；另新增 1 个测试文件。全部 6 个 `internal/standalone/web/*` 文件逐内容与 Claude 完成版一致，本次未改 Docker UI。

管理页仍调用 `/_admin/v1/update/{status,check,apply}`；Server 路由和独立 Bearer 管理鉴权仍生效；子 Hub 通过私有 Unix socket / 临时控制 token 调用同容器 launcher。默认固定 feed 是 `main/updates/standalone.json`，更新未被默认禁用，启动器的公钥与 `publisher/public.json` 一致。

这是一键确认的应用更新，不是无用户确认的静默升级，也不是 Docker daemon 自动更换容器。`/status` 不联网；点击检查才联网。接受 202 不是完成，更新任务受理回执先发送，再下载/验签/校验，验证完成前不停止旧 Hub。健康失败恢复旧程序；数据卷保留。无需 Docker socket 或第二个容器。

## 本地验证结果

- Node 管理页与公开 release preparation / verification 回归：最终 **14 项通过、0 失败、0 跳过**，包含新增的公钥身份不匹配和未签名 envelope 拒绝测试。
- 两份 Compose 的 `config --quiet` 均成功；使用合成 `relay.example.com` 地址，没有加载真实凭据或服务器配置。
- Windows 完整 `go test ./... -count=1`：launcher、standalone、签名工具通过；Hub 仅 Claude 新增 `TestPushSendsOneGenericMessagePerNews` 的 POSIX `0600` 权限断言失败，Windows 不实现这些权限位。本次不为了 Windows 改 Linux 运行安全行为。
- 排除上述一项平台断言后：`go test ./... -count=1 -skip '^TestPushSendsOneGenericMessagePerNews$'` 通过。
- Windows `go vet ./...` 通过；Linux amd64 与 arm64 交叉 `go vet ./...` 通过。
- 两架构 Hub 与 launcher 共 4 个交叉编译通过，编译版本 `0.4.0`。这些本地二进制不是 Docker 实机运行证据，不用于替代 CI 的正式产物。
- `git diff --check` 通过。复制本地快照带来的 SDK / 原插件文件换行差异不属于可提交范围；不能 `git add .` 混入这些路径。
- 产品拒绝、签名与篡改拒绝、下载中断不停止旧服务、feed 改变拒绝、健康失败回退、重新恢复签名状态、路径 fail-closed 及回执屏障均由现有与新增 launcher / standalone 测试覆盖。

## 尚待公开发布前执行

- 本机 Linux Docker daemon pipe 缺失，尚未本机构建/启动容器，也未本机跑 Linux race。必须运行 GitHub `Standalone release build` 并以真实结果补记。
- GitHub CI smoke 验证非 root、只读、初始化不得覆盖、health product / version / PID、管理鉴权、资源模式与重启持久化，但不调用真实更新源进行付费/真实签名升级。
- 正式 signed feed 需要对 **CI 成功的同 commit 产物** 用既有外部签名工具签名，并独立公钥复验；先上传并下载复验两个 Hub asset，再更新 main 的 standalone feed。
- GHCR 公共可拉取性、Release tag / commit / 二进制、最终远端清单必须再验；不能只看到 Actions 或上传命令成功就认为可升级。
- arm64 是交叉构建，不是 arm64 实机测试；真实公网 HTTPS、真实手机/电脑授权、FCM 与容量需部署环境验收。
- 应用回退不等于用户数据回滚；当前拒绝不同 `data_schema`。历史已验证程序保留会占用磁盘，尚无自动清理版本缓存承诺。

## 可提交范围（禁止把其它 dirty checkout 混入）

只 stage 本目录下这些路径：

- `.dockerignore`、`Dockerfile`、`compose.yml`、`compose.release.yml`、`README.md`；
- `.github/workflows/standalone-docker.yml`、`.github/workflows/standalone-release.yml`；
- `deploy/standalone/`；
- `docs/STANDALONE-DOCKER.zh.md`、`docs/STANDALONE-RELEASE.zh.md`、本记录；
- `plugins/salcara-hub/cmd/hub/`、`plugins/salcara-hub/cmd/hub-launcher/`；
- `plugins/salcara-hub/internal/hub/`、`plugins/salcara-hub/internal/standalone/`、`plugins/salcara-hub/internal/launcher/`（最新 Claude 内核加本次自动更新改动）；
- `plugins/salcara-hub/tools/standalone-feed/`。

不要 stage `output/`、宿主补丁、原插件 UI、SDK、个人版、私钥、用户数据、服务器配置。`output/` 全部是临时本地验证，不应上传。

CI/人工步骤见 `STANDALONE-RELEASE.zh.md`。源发布、GHCR image、GitHub Release、signed feed 是四步，不会重启或部署任何生产服务器。

## 第一轮远端 CI 与测试修正

- main 源提交 `3d8cc81b72bce64ada056c4f40a550805ee79f31`，运行 `37138695440`。
- Linux Docker build 内全包测试、vet、race 已通过；真实非 root/read-only 容器完成初始化、鉴权、资源模式、更新状态验证后，stop/start 健康探测失败，因此没有镜像推送或 Release。
- 测试使用 Docker 临时随机宿主端口；stop/start 后仍探测旧端口。测试脚本现每次启动后重新读取绑定，保留重启持久化全部断言，并在失败时输出仅新测试容器的状态和脱敏日志。没有修改 Hub 运行内核来绕过失败。
- 第一次测得闲置新容器 11.89 MiB / 限额 384 MiB；这不代表生产负载容量或峰值。
- 修正仍必须再跑 CI，以实际结果确认根因；不能因推测端口改变就宣称修复完成。

第二轮 `37139332886` 重启 smoke 全部通过，证实端口修正；两架构导出被 ELF 架构校验阻止：Dockerfile 给自动 `TARGETARCH` 指定 `amd64` 默认值覆盖了 buildx 的 arm64 参数，导致 arm64 输出是 x86 文件。构建参数改为无默认值的 Docker 自动平台 ARG；保留架构强校验，不发布错误镜像。运行内核未改。

## 最终正式 Docker 发布验收

- 最终运行源码 `b84f4e2913d3f78ea855e6539ff34aeebde47722`；GitHub Actions `37141444610` 的 build、publish-image 均成功。包括最新镜像/缓存选择及 Linux 真子进程回归、全包/vet/race、非 root/read-only 容器 smoke、重启持久化和双架构 ELF 检查。
- Actions artifact `11280692192`，下载 ZIP 26,059,613 字节，与 GitHub digest `sha256:9880031506f4bcb4563ca3cc4c1b1a4cf44215837b9a50f0c84f9cbdb3f2026a` 一致。使用既有仓库外私钥签名，固定公钥独立复验成功。
- 正式 Release `standalone-v0.4.0` 已创建并公开，标签准确指向上述源码。原插件 `v0.3.1` 仍保持仓库 latest，不把独立 Docker 冒充插件升级。
- 两个公开 Hub assets 和签名 feed 已重新下载到独立空目录，GitHub asset digest/size 与实际字节全部一致，再运行 `verify-release.mjs` 确认产品/签名/版本/两架构 hash 一致。
- amd64 Hub：8,376,444 字节，SHA-256 `996388d9241c26a2fd6bd145fa94ed58dcdd66510b91c3365b8e377b28eaefcb`；arm64 Hub：7,733,372 字节，SHA-256 `8b855ab053e92d4e01383ad6ed43f8752f8b6f0cc090b0ec292f3fecc5482d13`。
- GHCR 匿名 token 和 `0.4.0` OCI index 均返回 200。最终 index digest `sha256:e3b5f65957aefa8027cd18f725e73d287ce68a98f7f5d2df81f9cff9ce5dae31`；amd64 `sha256:066ea79351a8214c8fde2d814c40aea9301ba20bb28ae6a9a5e25616b70068e1`；arm64 `sha256:35cdf69038f71f7df74443ea5632cdb52a33570704477f10c3ef3e27103b743c`。两个 unknown/unknown 项为 provenance attestations，不是额外可运行平台。
- 签名清单仅在上述公开复验之后发布到 `main/updates/standalone.json`。未部署或重启生产服务器、未改个人版/原插件 feed/数据卷。arm64、真实公网设备、生产容量与 FCM 仍须实际环境验收。
