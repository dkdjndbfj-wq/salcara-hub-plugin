# 来源与第三方许可

本项目基于 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api)，遵循上游仓库 `LICENSE` 所载 LGPLv3（完整许可证保留在仓库根）。本项目新增或修改的插件/宿主适配不把上游许可证替换为 MIT。

插件 SDK 路径 `backend/pkg/pluginapi`、模块依赖清单 `backend/go.mod` / `backend/go.sum` 来自官方 v0.2.11：commit `96f4c115c9749078f90cbf210a01d39baf3f53b6`，Git tree `0e35899fa37140d852c17b9d4bd2558cf29e7c17`。清单 schema 的 Hub capability 扩展为本项目适配修改。此仓库只保留构建插件所需 SDK；配套完整宿主源码/patch 随发布资料交付。

主要运行依赖包含 HashiCorp go-plugin、gRPC、Protocol Buffers 及 `golang.org/x/*`，各自保留其模块许可。依赖的准确版本由 go.mod/go.sum 锁定，可通过 `go list -m all` 审核。实际 Windows/Linux 插件构建依赖的完整 LICENSE/NOTICE 等文件与 Go 标准库许可证保存在 [plugins/salcara-hub/licenses](plugins/salcara-hub/licenses)，完整 GPLv3 补充正文也在其中；签名插件包同样包含这些资源并声明文件哈希。自动 TLS 与协议实现不应移除安全校验。

这里没有捆绑桌面 Bridge、CLIProxyAPI、Node/Electron 或 Codex/Claude 软件本体；它们是不同组件，其许可不替代插件/宿主的 LGPLv3。
