# 来源与第三方许可

Salcara Hub 是独立部署的设备配对和远程消息服务。本仓库继续保留项目既有的 LGPLv3 许可及来源归属；产品名称、目录整理和删除不再使用的代码不构成重新授权。

历史版本包含来自 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) 官方 v0.2.11（commit `96f4c115c9749078f90cbf210a01d39baf3f53b6`）的接口代码。当前独立服务不再包含该接口代码及其依赖，不需要编译或修改 Sub2API。完整 LGPLv3 正文保留在仓库根 [LICENSE](LICENSE)，其引用的 GPLv3 正文保留在 [licenses/COPYING.GPL-3.0](licenses/COPYING.GPL-3.0)。

当前 Go 模块只使用 Go 标准库，无第三方模块依赖，可在仓库根执行 `go list -m all` 核验。Go 标准库的许可保留在 [licenses/Go-LICENSE](licenses/Go-LICENSE)。Docker 镜像同时附带上述许可与本文件；构建时使用的 Go 工具链及基础镜像各自继续遵循其上游许可。

本仓库不捆绑配套手机、桌面 App，也不捆绑 Codex、Claude 或其他 Agent 软件。配套产品与第三方软件各自遵循其许可，不替代本仓库的许可。
