# 插件二进制第三方许可资源

这些资源由实际 Windows amd64 与 Linux amd64 构建的 `go list -deps` 模块集合导出，不包含系统路径、密钥或私人配置。每个 dependencies 子目录名称对应模块路径和版本，文件保持原文。

模块：fatih/color v1.18.0，golang/protobuf v1.5.4，hashicorp/go-hclog v1.6.3，hashicorp/go-plugin v1.8.0，hashicorp/yamux v0.1.2，mattn/go-colorable v0.1.13，mattn/go-isatty v0.0.20，oklog/run v1.1.0，golang.org/x/net v0.58.0，golang.org/x/sys v0.47.0，golang.org/x/text v0.41.0，google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa，google.golang.org/grpc v1.83.2，google.golang.org/protobuf v1.36.11。

Sub2API SDK 与本项目适配沿用仓库根 LGPLv3 LICENSE。COPYING.GPL-3.0 是 LGPLv3 援引的完整 GPLv3 正文；Go-LICENSE 是 Go 标准库许可证。完整对应源码在公开仓库及配套宿主源码发布资料中提供，模块依赖由 go.mod/go.sum 锁定并可从公开源获取。
