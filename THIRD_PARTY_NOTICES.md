# 来源与第三方许可

可选节点服务器地址归属使用 [DB-IP Lite](https://db-ip.com/db/lite.php) 国家与 ASN 月度数据库，数据来源署名 **IP Geolocation by DB-IP**，遵循 [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/)。数据库由管理员启用后从官方地址下载，不随镜像捆绑；本地解析使用 `github.com/oschwald/maxminddb-golang v1.13.1`，ISC 许可保留于 [licenses/MAXMINDDB-LICENSE](licenses/MAXMINDDB-LICENSE)。Lite 数据精度有限，服务器地址归属不等于代理实际出口或用户所在地。

Salcara Hub 是独立部署的设备配对和远程消息服务。本仓库继续保留项目既有的 LGPLv3 许可及来源归属；产品名称、目录整理和删除不再使用的代码不构成重新授权。

历史版本包含来自 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) 官方 v0.2.11（commit `96f4c115c9749078f90cbf210a01d39baf3f53b6`）的接口代码。当前独立服务不再包含该接口代码及其依赖，不需要编译或修改 Sub2API。完整 LGPLv3 正文保留在仓库根 [LICENSE](LICENSE)，其引用的 GPLv3 正文保留在 [licenses/COPYING.GPL-3.0](licenses/COPYING.GPL-3.0)。

当前 Go 模块使用 Go 标准库，以及用于安全解析订阅节点的 `go.yaml.in/yaml/v3 v3.0.4`，可在仓库根执行 `go list -m all` 核验。YAML 上游许可与声明保留在 [licenses/YAML-LICENSE](licenses/YAML-LICENSE) 和 [licenses/YAML-NOTICE](licenses/YAML-NOTICE)，其源文件按上游声明分别受 MIT / Apache 2.0 许可约束。Go 标准库的许可保留在 [licenses/Go-LICENSE](licenses/Go-LICENSE)。Docker 镜像同时附带上述许可与本文件；构建时使用的 Go 工具链及基础镜像各自继续遵循其上游许可。

本仓库不捆绑配套手机、桌面 App，也不捆绑 Codex、Claude 或其他 Agent 软件。配套产品与第三方软件各自遵循其许可，不替代本仓库的许可。

SSH 主机接入使用 `golang.org/x/crypto v0.58.0`，并间接使用 `golang.org/x/sys v0.49.0`。BSD 许可分别保留在 [licenses/X-CRYPTO-LICENSE](licenses/X-CRYPTO-LICENSE) 与 [licenses/X-SYS-LICENSE](licenses/X-SYS-LICENSE)。未引入 vm2api 源码或依赖。

持久消息存储使用 `modernc.org/sqlite v1.39.1` 及其 libc/mathutil/memory、go-humanize、go-isatty、go-strftime、bigfft、x/exp 依赖；SQLite 本体为公有领域，Go 移植与依赖各自遵循其上游声明。执行身份使用 `github.com/google/uuid v1.6.0`，图片头解析和受限缩略图使用 `golang.org/x/image v0.32.0`。上述依赖完整许可和附属声明保留于 [licenses/durable](licenses/durable/)，Docker 镜像一并附带。确切版本以 `go.mod` / `go.sum` 为准。

可选受管代理内核为 [MetaCubeX/mihomo v1.19.33](https://github.com/MetaCubeX/mihomo/tree/v1.19.33)，上游采用 GPL-3.0，许可正文同 [licenses/COPYING.GPL-3.0](licenses/COPYING.GPL-3.0)。Edge 镜像不捆绑该二进制；管理员启用安装时从官方 release 下载固定平台产物，并核对内置 SHA-256。完整版本、校验值和出站适配范围见 `internal/edgepool/kernel.go` 与 [使用说明](docs/MANAGED-PROXY-POOL.zh.md)。若另行再分发内核，应附上其许可、对应源代码及 GPL 要求的信息；本项目许可不替代内核许可。未复制或捆绑 vm2api 代码。
