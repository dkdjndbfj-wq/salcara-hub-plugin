# Salcara Hub：独立服务与插件

独立站点部署的电脑/手机扫码配对与远程消息转发服务。不要求本站账号、模型 API Key、余额或特定模型分组。站点不同，配对凭据独立。

## 推荐：独立 Docker + 可视化侧边栏

新部署推荐 [独立 Docker 安装说明](docs/STANDALONE-DOCKER.zh.md)：单独运行 Hub，将 `/salcara-hub/` 反代到独立容器，再把同源管理页添加到 Sub2API 的自定义 iframe 侧边栏。**无需修改官方 Sub2API 或安装服务器插件**；Sub2API 与 Hub 分别升级。0.5.0 管理员只输入随机初始管理密钥登录，可以在后台更换管理密钥；不需要账号或手输管理令牌，不拿模型 Key 或 Sub2API iframe token 当管理员权限。

提供本地审阅后执行的 [首次安装脚本](deploy/standalone/install.sh) 和 [Agent 部署指南](docs/AGENT-DEPLOY.zh.md)。脚本仅拉固定正式镜像，不修改 Nginx、不安装/重启 Docker、不覆盖已有配置或数据卷，结束后列出管理/iframe URL 和受限的初始管理密钥文件位置。旧 0.4.0 须按文档备份后通过完整镜像迁移，不能重新首次安装或用旧管理页应用更新跨过认证变更。

独立源码包含 [Dockerfile](Dockerfile)、[源码 Compose](compose.yml)、[正式镜像 Compose](compose.release.yml)、[Nginx 路径片段](deploy/standalone/nginx-location.conf) 和 [发布构建 CI](.github/workflows/standalone-release.yml)。**一个 Hub 容器**内的非 root 启动器支持网页检查并一键更新签名 Hub 应用，无需第二个更新器容器、不挂 Docker socket；底层镜像/启动器仍通过 Compose 更新，二者不能混称。正式镜像和签名清单以 `standalone-v版本号` 的 [Releases](https://github.com/dkdjndbfj-wq/salcara-hub-plugin/releases) 为准；未完成发布的版本不能当作可下载产物。见 [发布与更新维护](docs/STANDALONE-RELEASE.zh.md)。

控制台可选择“省资源 / 均衡 / 高负载”，默认省资源，真实调整暂存与新请求并发，确认后持久化，不取消已在电脑执行的任务。默认整个容器 384 MiB 是硬配额而非实测占用；Hub Go 软目标分别为 96 / 192 / 320 MiB，页面切换不会改 Docker 硬限，也不是用户数量承诺。父启动器软目标另为 32 MiB；应用升级只重启 Hub，会短暂中断远程连接。更新会先验固定发布者签名、大小/哈希、启动/数据协议，失败不启动未验证程序；程序回退不是用户数据回滚。

**官方 0.2.11 自定义 iframe 会向 URL 追加本站登录 token**。只使用自己控制的同源 HTTPS 页面，按文档禁用该路径 query 日志，并检查 CDN/WAF；Hub 收到静态页参数会先 303 清除，不把它当 SSO。菜单“管理员可见”不是 API 鉴权。

## 已发布插件：保留为旧部署方式

**原版 Sub2API 0.2.11 不能直接安装此 Hub 插件。** 它只接受 `openai.oauth.outbound_transport.v1`；本插件需要配套 `salcara.hub.http.v1` 宿主扩展。不要为了安装插件降级服务器，不要修改 capability 字符串冒充 OAuth 插件。

插件版本 0.3.1 / Hub 内核 1.4.0，目标为基于官方 0.2.11 的自定义 `0.2.11+salcara.hub.1`。生产部署、反代/TLS、真实手机远控仍须独立验收。Codex 原桌面续聊是需显式安装授权的实验能力；Claude Desktop 普通聊天/Cowork 远控尚未完成，不用 CLI 冒充桌面会话。

## 下载与信任

下载文件以 [Releases](https://github.com/dkdjndbfj-wq/salcara-hub-plugin/releases) 中实际发布为准。仅公开插件、文档和配套宿主适配；手机软件仍在独立私有仓库测试，不通过此仓库发布 APK。

发布者 ID 为 `salcara-local-20260930`。公开身份见 [publisher/public.json](publisher/public.json)，将 [公钥信任示例](publisher/trusted-publisher.example.yaml) 的条目合并到现有配置，保留 `allow_unsigned:false` 与其它原配置。私钥不在此仓库、插件包、GitHub Actions 或服务器中。此签名是 Salcara 独立发布者签名，不是 Sub2API/OpenAI 官方认证。

完整插件说明见 [插件说明](plugins/salcara-hub/README.md)，先阅读 [0.2.11 宿主适配与安装顺序](docs/HOST-UPGRADE.zh.md) 和 [检查更新/一键更新](docs/PLUGIN-UPDATES.zh.md)。[精确基线补丁](docs/host-adaptation-v0.2.11.patch) 只针对指定官方 commit，不对生产 dirty checkout 强行应用。签名不能给原版宿主增加能力。

## 从源码构建

此仓库的 `backend/` **仅包含插件 SDK 与其模块文件，不是完整 Sub2API 服务器**。SDK 来源和许可证见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。完整宿主源码与适配 patch 随对应 Release 提供，不应对 dirty 生产 checkout 强行应用。

```sh
cd plugins/salcara-hub
go test ./...
go vet ./...
# 默认仅输出未签名开发包，不用于生产：
go run ./tools/package -output dist/salcara-hub-0.3.1.s2plugin
```

签名需使用你自己的仓库外私钥与独立 publisher ID。`tools/keygen` 创建全新身份，不覆盖旧密钥。`tools/release-feed` 只用公开身份验证已签名包、所有文件哈希，再生成 `update.json`，不读取私钥、不联网、不发布。

## 隐私与连接

当前不是端到端加密：HTTPS 保护传输，站点可读取转发内容，使用者须信任站点。手机按需读取/发送，不保持默认常驻 SSE；电脑因 NAT/唤醒仍有轻量连接。断线重试有时限与幂等性边界，不是永久 exactly-once 或永久云端聊天备份。

许可证遵循原 Sub2API 的 LGPLv3 及各依赖许可，见 [LICENSE](LICENSE)。公开仓库不应包含真实模型 Key、设备秘密、手机 token、私钥、管理密钥或服务器配置。
