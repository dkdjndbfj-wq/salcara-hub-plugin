# 2026-10-04 Docker 镜像升级与签名缓存交接

本次只修更新内核；未修改手机、桌面、Docker 前端界面或动效，未碰个人版、真实设备、服务器配置及发布私钥。固定发布者、产品、公钥、签名清单、数据 schema 和版本协议保持原有边界。

## 修复的问题

已通过网页更新的 Hub 会在持久卷记录一个签名缓存程序。原启动流程总使用该记录，后续 Docker 镜像内含更高版本 Hub 也不会生效。

## 修改范围

- `plugins/salcara-hub/internal/launcher/store.go`：加载时继续验证现有 Current / Previous 程序；新增只在内置版本更高时选择新镜像的候选决策。候选选择不写状态。新增可选的 `failed_bootstrap`（严格语义版本与 64 字符十六进制 SHA-256），仅用于抑制同一坏镜像的重复尝试，不接受文件路径、不决定任意程序执行。
- `plugins/salcara-hub/internal/launcher/startup.go`：把启动事务提取为可回归测试的内核。新镜像先经现有 runner 的产品 / PID / 版本 / 健康验证；成功才保存 Current，Previous 留旧签名缓存。失败且旧程序恢复健康后才保存回退状态；中断或两版均失败保持原确认状态。提交失败会停止未提交程序，不继续宣传成功。
- `plugins/salcara-hub/internal/launcher/run_linux.go`：调用该启动事务，随后按已保存的最终状态启动私有控制服务。现有唯一写入者、锁、Unix socket、控制 token、监督重启与停止屏障保持不变。
- `plugins/salcara-hub/internal/launcher/startup_test.go`：覆盖健康之前不提交、高版镜像生效且保留签名缓存、旧 / 等版镜像不降级、坏镜像回退与重启不重复尝试、新 hash / 版本可重试、坏缓存 / 坏签名 / 不兼容 schema 不可绕过、取消 / 两版失败不改确认状态、状态保存失败停止未提交子程序、失败元数据严格边界。
- `plugins/salcara-hub/internal/launcher/process_linux_test.go`：测试专用子进程 fixture 可区分镜像 / 缓存版本；新增实际 Linux 已验证 inode 执行、产品 / PID / 版本健康与坏镜像先停止再恢复签名缓存的回归。不向发布 launcher 增加 fixture 接口。
- `docs/STANDALONE-RELEASE.zh.md`：补充整镜像升级行为和历史更新缓存的磁盘限制。

## 验证与发布约束

便携事务回归可在 Windows 运行；实际 Linux 子进程测试必须由 Linux GitHub CI 执行，不以交叉编译当实机通过。最终发布必须使用包含这些修改的同一固定 commit 重新构建、验签、导出与核对产物；先前 CI 产物不能冒充这次修复后的版本。

`failed_bootstrap` 是 schema 1 中的可选字段，兼容没有该字段的旧状态；旧启动器的严格 JSON 解码可能拒绝读取这个新字段，不能承诺可以随意换回旧镜像。用户数据不会被升级或回退逻辑清空。历史二进制不自动清理，单个下载上限 64 MiB、总量随版本累加，详见发布维护文档。

Linux runner 若健康失败但无法确认旧子进程停止，仍会拒绝第二个写入者。程序回退不代表数据 schema 回滚；本次未放宽 schema 验证。

## 本地已执行结果

- 独立标准库 audit 拷贝中的 `go test ./... -count=1`：launcher 与 standalone-feed 全部通过，无跳过。该拷贝不包含个人版或非更新业务内核。
- 同拷贝 Windows `go vet ./...`、Linux amd64 / arm64 `go vet ./...` 均退出 0；Linux amd64 launcher 测试二进制交叉编译通过。
- 这不是实际 Linux runtime 通过结论；新增两个真实子进程回归仍需最终 GitHub Linux CI 的 test / race 成功后确认。
- release 源六个 `internal/standalone/web/*` 文件与 Claude 最新 `output/docker-kernel-review/internal/standalone/web/*` 的 SHA-256 全部相同。
- `git diff --check` 退出 0；git 仅报告已有 Windows 换行转换提醒。仅提交本文件列出的七个源码 / 测试 / 文档路径，不混入未归属的 SDK、原插件与 license 换行变化。
