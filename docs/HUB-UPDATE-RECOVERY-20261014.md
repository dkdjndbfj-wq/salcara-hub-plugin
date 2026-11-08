# Hub 更新中断、重启与恢复专项（2026-10-14）

本轮只修改 launcher 的进程锁生命周期，新增默认 Linux 进程回归与显式启用的实际 Hub 安装/升级实验。保留工作区既有修改；不改管理页、不发布、不签正式资产、不使用发布私钥、真实账号或既有业务容器。

## 已复现并修复

原来持久卷的 `.launcher.lock` 只由 launcher 持有，并设置 `CLOEXEC`。launcher 遭到 `SIGKILL` 时该锁立即释放；其 Hub 子进程虽然接到 `Pdeathsig=SIGTERM`，仍可能在处理优雅退出。此时另一个 launcher 已能拿锁并开始重建控制通道、尝试启动程序，早于旧子进程真正退出。

`standalone.New` 原本另有独立的数据锁，因此该现象**不是已复现真实用户数据双写**；第二个实际 Hub 通常会被数据锁拒绝。本次修复把 launcher 排他期限扩展到被监管子进程的完整生命周期，避免过早的监管器接管、控制凭据重建和启动竞态。

`Run` 现在把已有锁交给 `managedRunner`，执行已验证 inode 时，通过额外文件描述符 4 让子进程继承**同一个** flock。父进程在正常升级/回退期间持续持有自己的描述符，子进程正常退出不会解除父进程的锁；父进程被杀后，子进程仍持锁直到实际退出。没有新增任意可执行路径、生产配置、网络权限或 Docker socket。

默认 Linux 回归 `TestKilledLauncherKeepsVolumeLockedUntilChildFinishes` 使用新编译测试程序启动真实监管器/子进程，然后实际 `SIGKILL` 监管器：

- 修复前红灯：`another launcher acquired the volume while the orphan Hub still drained writes`（0.17 秒）。
- 修复后：先确认旧子进程已经收到退出信号且仍存在；第二次拿锁被拒；允许旧子进程退出后才可重新拿锁。
- `TestRunnerStartsVerifiedInodeAndWaitsBeforeReplacement` 同时验证正常 Stop/Start 间父进程锁不释放，合法接替不死锁。

27 项默认 launcher 顶层测试已在真实 Linux 容器执行通过，无 skip，包含进程/PID/产品校验、镜像升级、签名缓存回退、取消启动、提交失败和失败镜像不反复重试。该数字不重复累加子用例或复跑次数。

## 实际 Hub 安装与升级证据

新增 `internal/launcher/installed_recovery_linux_test.go`，仅在 `linux && recoveryintegration` 显式构建；普通测试不会静默跳过它，也不会尝试访问其固定控制路径。显式实验要求提供两个真实 Hub 二进制；缺失输入直接失败。控制令牌使用 `O_EXCL` 创建，已有文件不会被覆盖。

2026-10-14 使用 Go 1.27.1 从**同一份本轮源码**构建两个 Linux/amd64 Hub，仅把编译版本标识设为 0.5.2、0.5.3。在非 root（65532）、断网、只读 rootfs、全部 capability 移除、禁止提权、64 PID / 384 MiB / 1 CPU 的一次性容器中，运行实际 `Hub -init`、HTTP 登录、管理更新请求、Unix 控制代理和实际 Hub 子进程；每次程序执行均经过产品/版本/PID/健康校验。

最后一轮 `TestInstalledHubUpgradeRecovery` **通过，22.79 秒**：

1. 初始化新的实际管理凭据并启动真实 0.5.2 Hub；写入仅供本轮使用的合成配对、项目、封禁记录，经实际运行程序/API 验证可读；通过管理 API 保存资源与自动清理设置。
2. 经 HTTP 管理更新接口提交测试签名的 0.5.3 清单；实际停止旧 Hub、执行下载缓存、确认新版、提交状态。旧管理 cookie 失效，同一管理密钥可重新登录。
3. 停止并重新启动真实 Hub；从磁盘验签恢复 0.5.3 缓存，未因旧 bootstrap 为 0.5.2 而降级。
4. 下载响应声明完整签名大小，但正文只返回一半即 EOF。更新失败，原 Hub 和已登录会话继续有效。
5. 下载签名/哈希有效、声明 0.5.4 但实际健康版本为 0.5.2 的真实 Hub 程序；健康验证失败后停止该进程，恢复已确认 0.5.3。

每阶段检查管理凭据文件字节一致；合成手机配对 token 经真实 HTTP API 仍可访问绑定设备及项目 metadata；管理 API 中配对、封禁和资源/清理设置均保留。配对记录来自合成持久化种子，不把它当作真实手机扫码或桌面会话验证。

测试签名用随机临时 Ed25519 密钥；feed 和下载响应由测试 transport 提供，测试网络被禁用。**两份程序版本标识不同，但源码相同**：这是实际安装、进程切换、缓存恢复与失败回退链路证据，不能声称历史已发布 0.5.2 到未来版本的数据迁移兼容性已验证，也不代表固定生产发布者签名、HTTPS 下载、ARM 或正式镜像升级已验收。

实际二进制 SHA-256（小写）：

| 编译版本标识 | SHA-256 |
| --- | --- |
| 0.5.2 | `1b7414b7ed3a53cc71289d5b80db984c5aa09480502a07a89f83cd2dd8495598` |
| 0.5.3 | `a70f05a677311ab85370764a745b924a39d1347e897355b0fb6fb5a9687456a4` |

## 复跑

从仓库根目录，在具有 Go 1.27.1 与本地 Alpine 镜像的环境中构建到新的 `work` 子目录。下面是 POSIX shell 示例；Windows 可用同等参数交叉构建，Docker bind mount 使用对应绝对路径。

```sh
mkdir -p work/hub-update-recovery
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags='-s -w -X salcara/hub/internal/standalone.Version=0.5.2' \
  -o work/hub-update-recovery/hub-old ./cmd/hub
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags='-s -w -X salcara/hub/internal/standalone.Version=0.5.3' \
  -o work/hub-update-recovery/hub-next ./cmd/hub
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -tags recoveryintegration -c \
  -o work/hub-update-recovery/recovery.test ./internal/launcher

docker run --rm --network none --read-only --user 65532:65532 \
  --cap-drop ALL --security-opt no-new-privileges \
  --pids-limit 64 --memory 384m --cpus 1 \
  --mount "type=bind,source=$PWD/work/hub-update-recovery/recovery.test,target=/fixture.test,readonly" \
  --mount "type=bind,source=$PWD/work/hub-update-recovery/hub-old,target=/hub-old,readonly" \
  --mount "type=bind,source=$PWD/work/hub-update-recovery/hub-next,target=/hub-next,readonly" \
  --tmpfs /tmp:rw,exec,nosuid,nodev,size=192m,mode=1777 \
  -e SALCARA_RECOVERY_OLD_HUB=/hub-old \
  -e SALCARA_RECOVERY_NEXT_HUB=/hub-next \
  alpine:latest sh -c 'cp /fixture.test /tmp/fixture.test && chmod 500 /tmp/fixture.test && /tmp/fixture.test -test.run=TestInstalledHubUpgradeRecovery -test.v -test.timeout=90s'
```

需要执行默认的全部 launcher 测试时，普通 `go test -c` 构建测试程序，再在同样的隔离 Linux 容器中执行，不启用 `recoveryintegration`。Windows bind mount 文件可能显示 group/world writable，因此先复制到容器自己的临时目录并 chmod，再让已验证 inode 执行检查接受它；未放宽生产文件校验。

本轮临时容器均使用 `--rm`，没有命名持久卷或网络映射，未接触 Docker socket、现有业务容器或真实密钥。容器退出后临时安装数据及测试令牌一起销毁；本地编译输入保留用于独立复验。

## 仍需分开验收

根任务负责冻结源码后的完整 normal/vet/race 与正式 Dockerfile 构建/容器 smoke。本专项不替代那组检查，也不把既有测试的重复执行叠加为新增数量。历史真实发布产物的跨版本升级、正式发布者离线签名、HTTPS 资产与多架构镜像/ARM 实机门槛，仍按 `STANDALONE-RELEASE.zh.md` 和 `RELEASE-CONTRACT-AUDIT-20261014.md` 单独留证。
