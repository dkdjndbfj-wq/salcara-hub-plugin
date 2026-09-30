# Salcara Hub 插件

独立站点部署的电脑/手机扫码配对与远程消息转发插件。不要求本站账号、模型 API Key、余额或特定模型分组。站点不同，配对凭据独立。

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

许可证遵循原 Sub2API 的 LGPLv3 及各依赖许可，见 [LICENSE](LICENSE)。公开仓库不应包含真实模型 Key、设备秘密、手机 token、私钥或服务器配置。
