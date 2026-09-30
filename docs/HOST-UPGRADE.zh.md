# Salcara Hub 宿主适配：安装报错与离线升级资料

> 适用于官方 **Sub2API `v0.2.11`** 的独立适配资料。如果你使用的“2.11”是不同仓库 / 分叉，应先核对来源和接口；不应降级到早期 `0.2.8` 研究基线。该资料不自动部署、不自动信任发布者。

## 为什么上传插件会显示“初期仅支持能力 openai.oauth.outbound_transport.v1”

这是**宿主能力不兼容**，不是签名错误。该提示来自原版 `PluginManifest.Validate()`：此版 Sub2API 仅允许 OpenAI OAuth 上游传输插件。Salcara Hub 提供设备配对、远程 HTTP 转发和管理员设备操作，声明的是独立的 `salcara.hub.http.v1`。

签名只证明发布者身份与文件未被篡改。添加可信公钥不会增加路由、运行时和权限接口。不能把 Hub 改名成 OAuth 能力来绕过检查，这会混淆权限且仍缺少实际实现。

需要依次完成：升级带 Hub 适配的宿主 → 追加并核对发布者公钥 → 上传签名 `.s2plugin` → 检查配置并启用 → 验证 `/salcara-hub/v1/ping`。

## 本资料的精确版本与边界

- 源码基线：官方 tag [`v0.2.11`](https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.11)，commit `96f4c115c9749078f90cbf210a01d39baf3f53b6`。
- 精确源码验证：通过官方 commit 范围的完整最终 diff 重建，移植前 `git write-tree` 与官方 GitHub API 的完整 tree SHA `0e35899fa37140d852c17b9d4bd2558cf29e7c17` 相等，原始签名 commit 对象的 SHA 也逐字节复核相等。
- 自定义构建标记：`0.2.11+salcara.hub.1`；这不是官方支持 Hub 的发行版，原版 `v0.2.11` 的清单校验仍然只允许 OAuth 传输能力。
- 官方 tag 内 `backend/cmd/server/VERSION` 文件为 `0.2.10`；官方发行流程注入 tag 版本。本适配也必须按下面命令注入 `0.2.11+salcara.hub.1`，不能仅无参数编译后误称当前版本。
- 插件能力：`salcara.hub.http.v1`，协议 / 传输 API / UI Bridge 均为 v1。
- 兼容前提：此基线已有插件安装器、插件数据库表、gRPC 协议、管理员权限和 step-up 中间件。
- 包含新增 Hub 路由、独立生命周期 / 数据目录、私有管理 API、mTLS 和宿主插件页桥接。没有新增数据库迁移；基线自带迁移仍可能在启动时执行。
- 这是离线构建和定向测试资料，尚未验证你的生产数据库、反代、管理员登录或公网远控。没有修改、发布或重启任何线上站点。
- 签名包的本地安装器与宿主 → gRPC → Hub HTTP 集成测试在 Windows loopback 环境完成，使用隔离目录和测试凭据，包含 mTLS 未授权客户端拒绝检查。此结果不是 Linux 服务器实机启动、生产数据库或手机公网端到端验收；清单中已测试的自定义版本也不代表原版官方 `v0.2.11` 自动支持 Hub。

`host-adaptation.patch` 仅针对上述官方 `v0.2.11` commit，补丁不能保证适用于其他原版 / 分叉版本。完整宿主源码快照不含插件私钥、个人配置或 `plugins/` 目录；插件自身源码与签名包单独提供。修改遵循仓库 `LICENSE`（LGPLv3）及各依赖许可证。

## 两种部署方式

先确认你服务器用的 CPU 架构（`uname -m`：`x86_64` 对应 amd64，`aarch64` 对应 arm64）。不要把宿主升级和首次插件安装直接当作生产验收。

### A. 使用预编译宿主包制作自己的 Docker 镜像

按架构解压 `sub2api-salcara-host_0.2.11+salcara.hub.1_linux_<架构>.tar.gz`。包内包含 `sub2api`、fallback `backend/resources`、原部署 entrypoint、`Dockerfile` 和本说明。管理前端及数据库迁移已嵌入二进制；不需要另外替换 nginx 静态网页。

在解压目录执行（以下是手动操作示例，本次没有执行部署）：

```sh
sha256sum -c checksums.txt
docker build --platform linux/amd64 -t salcara/sub2api-hub:0.2.11-hub1 .
```

ARM 服务器将平台改为 `linux/arm64`。`Dockerfile` 沿用该基线的 Alpine / PostgreSQL-client 镜像构建流程，构建需联网拉取基础镜像，**包不是 `docker load` 格式的预制镜像**。其镜像名称只是你本地的标签，不会自动上传。

先在隔离副本中验证配置、数据库迁移、API、登录和备份恢复。确认无误后，由运营者在现有 Compose 的 Sub2API **同一个服务**中更换 image，保留原 Postgres / Redis / 配置 / 数据卷 / JWT 与加密密钥，不新建站点、不运行 `down -v`。不要将完整原配置替换成公钥示例。

保留旧 image tag、原 Compose 文件和配置；升级前做可恢复的数据库及插件数据备份。恢复旧镜像不一定能回退已执行的数据库迁移，因此回滚要连同兼容的数据备份一起规划。不要未经确认执行全站“自动更新到官方 latest”，它可能覆盖此自定义能力。

### B. 自己审查源码并从源码构建

完整源码快照包含此基线和所列适配。也可在**干净的上述 commit 副本**中审查 `host-adaptation.patch` 后执行 `git apply --check`，通过后再应用；不要对有用户修改的生产 checkout 直接强行打补丁。

从源码目录使用原多阶段 Dockerfile：

```sh
docker build --platform linux/amd64 \
  --build-arg VERSION=0.2.11+salcara.hub.1 \
  --build-arg COMMIT=96f4c115-salcara-hub-adaptation \
  -t salcara/sub2api-hub:0.2.11-hub1 .
```

它会构建 Vue 前端并以 `-tags embed` 嵌入 Go 服务；不要只编译一个未嵌入前端的后端，再继续使用旧网页。要做双平台发布，用你自己的 buildx 环境分别构建并验收，不把 amd64 程序塞进 arm64 镜像。

## 签名信任配置

宿主已有 `plugins.trusted_publishers` 配置，通常需要在服务器配置中追加并重启宿主生效，不是点击某个“忽略报错”按钮。使用签名交付目录里的 `trusted-publisher.example.yaml` 的**公钥**和对应 `key_id`；核对发布者公钥指纹。不要上传 / 共享 `.private`、`.key` 或其他私钥文件。

```yaml
plugins:
  allow_unsigned: false
  trusted_publishers:
    YOUR_PUBLISHER_KEY_ID: "BASE64_ED25519_PUBLIC_KEY"
```

把这两个字段合并进原 `plugins` 段，不重复顶层段、不删除已有配置。签名不是微软 / 苹果代码签名证书，也不是官方 Sub2API 认证；你信任的是此发布者的 Ed25519 公钥。未签名模式仅适合隔离测试，不建议为了解决能力错误关闭生产签名检查。

## 插件“检查更新”和“一键更新”

此宿主适配的管理员插件卡片增加了两个按钮：**检查更新**（手动发起）和发现兼容新版本后的**更新至某版本**。没有后台定时检查 / 自动换包，不会更新手机 APK，也不会自动升级 Sub2API 宿主。

Salcara Hub 默认更新清单地址为 `https://github.com/dkdjndbfj-wq/salcara-hub-plugin/releases/latest/download/update.json`，固定发布者为 `salcara-local-20260930`。必须先按上节核对并添加该发布者的公钥；更新清单不能授予信任。若发布者尚未发布清单，页面会明确显示“尚未提供公开更新清单”，不能假报更新成功。

其他运营者可在原 `plugins` 段内覆盖该插件的更新源（仅服务器配置可改，插件 UI 不能写入）：

```yaml
plugins:
  update_sources:
    top.salcara.hub:
      url: "https://你的公网发布站/update.json"
      publisher_key_id: "YOUR_PUBLISHER_KEY_ID"
```

显式把该条目的 `url` 设为空字符串，可禁用内置源并继续手动上传。更新只允许公网 HTTPS / 443，下载及重定向会检查目标 IP，拒绝内网、环回和云元数据地址；不携带服务器 API Key、Cookie 或代理凭据。若部署环境仅能通过 HTTP 代理联网，此严格直连下载器可能无法访问 GitHub，应配置可直接访问的受信任公网镜像或手动上传，而不是关闭安全检查。

更新时会重新检查清单、版本、固定发布者、包大小 / SHA-256、签名文件、能力和宿主兼容性；验证失败不会停用原插件。换包要求管理员二次验证，并提示运行中连接会短暂断开；旧的停用插件更新后仍停用，原来启用的插件尝试恢复运行。原安装 ID、加密配置、绑定配置和宿主插件存储数据保持，Hub 的配对 / 封禁状态须放在同一持久化数据目录中。仍应提前备份，不能承诺第三方未来版本的数据格式始终向后兼容。

若换包阶段失败或新进程无法启动，页面会分别提示“状态不确定 / 可能已停用”或“已安装但恢复失败”；这不是自动回滚。刷新状态后处理，不自动重放更新请求，也不删除 Hub 数据来“修复”。宿主扩容多副本时也需重新验收；该更新实现有版本 / 状态条件校验，但不能代替整个站点的部署锁与数据备份。

## 上线前的最小检查

1. 宿主版本显示自定义构建，插件上传不再出现 OAuth-only capability 错误。
2. 签名验证通过；未知公钥、篡改清单 / 文件仍然被拒绝。
3. 插件配置的数据目录可写且持久化；只允许单个 Hub 实例管理该目录。
4. 插件启用后请求 `https://你的站点/salcara-hub/v1/ping` 返回 `service=salcara-hub`、`protocol=salcara-remote`、`protocolVersion=1`，不是网页 / 404。
5. 反代允许该前缀 HTTP 和电脑 SSE 流，关闭对应流响应缓冲并保留合理超时；公网 TLS / 限流需站点自行配置。
6. 用无生产秘密的测试设备验证扫码、撤销、跨设备隔离、封禁、离线和断线恢复；管理员私有接口不能被公网伪造。

## 必须保留的宿主改动

后端：

- `backend/internal/server/router.go`
- `backend/internal/server/routes/admin.go`
- `backend/internal/handler/admin/plugin_hub_handler.go`
- `backend/internal/service/plugin_hub.go`
- `backend/internal/service/plugin_manager.go`
- `backend/internal/service/plugin_manifest.go`
- `backend/internal/service/plugin_runtime.go`
- `backend/internal/service/plugin_package.go`
- `backend/internal/config/config.go`
- `backend/internal/repository/plugin_update_repo.go`
- `backend/internal/handler/admin/plugin_update_handler.go`
- `backend/internal/service/plugin_update.go`
- `backend/internal/service/plugin_update_http.go`
- `backend/pkg/pluginapi/v1/manifest.schema.json`

前端：`frontend/src/api/admin/plugins.ts`、`frontend/src/views/admin/PluginsView.vue`、`frontend/src/components/plugins/PluginUpdater.vue`、`frontend/src/i18n/locales/{zh,en}/admin/plugins.ts`。

`backend/internal/server/routes/gateway.go` 的旧 `/v1/salcara-hub/key` 验证接口也包含在此基线适配补丁中，但**新设备扫码模式不依赖它**。不要把旧模型 API Key 与新设备 / 手机配对凭据混用。

回归测试与本构建脚本也随源码附带。mTLS、严格能力验证、公网 / 私有路径隔离、管理员权限与 step-up 链不可省略；只增加清单中一个 capability 字符串不够。
