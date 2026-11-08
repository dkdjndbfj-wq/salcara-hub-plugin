# 2026-11-08 三端 GitHub 发布验收

三端均已公开发行，原开发工作区的未提交修改与暂存状态保持不变。本记录只记录已观测结果，不把 GitHub 发布当作服务器部署或实体手机验收。

| 产品 | 版本 | 发行页 | 构建源码 |
| --- | --- | --- | --- |
| Android | 1.8.3，versionCode 24 | https://github.com/dkdjndbfj-wq/salcara-image-mobile/releases/tag/v1.8.3 | d4f93bce4e3f264009604ed85f73a9f896369047 |
| Desktop Windows x64 | 1.6.5 | https://github.com/dkdjndbfj-wq/salcara-desktop/releases/tag/v1.6.5 | 6add9977aa94836d888bd850e24585f1d4672e20 |
| Hub | 0.5.3 | https://github.com/dkdjndbfj-wq/salcara-hub-plugin/releases/tag/standalone-v0.5.3 | 8aa7c351e9d0018fca0e19778d6ad1af0690dfb5 |

发布时间分别为 2026-11-08 13:45:47、13:45:39、13:45:43 UTC。三仓库 main 均先以非强制快进合入通过检查的候选；Hub main 随后仅更新签名 feed，并增加本验收文档。发行标签保持在各自精确构建源码。

## 构建与复核

- Android 正式签名构建：[42477859024，attempt 2](https://github.com/dkdjndbfj-wq/salcara-image-mobile/actions/runs/42477859024)。335 套测试通过、8 套跳过；4792 项通过、8 项跳过。官方 Expo 依赖检查 21/21；类型检查通过。原生构建、与前版覆盖安装签名兼容性、包名及版本检查通过。
- Android 独立复核：[42481434345](https://github.com/dkdjndbfj-wq/salcara-image-mobile/actions/runs/42481434345)。ZIP CRC、ARM64/ARMv7 的 52 个原生库及 ELF 架构、既有正式签名证书一致性通过。
- Desktop 同源 Linux 全量检查：[42485009684](https://github.com/dkdjndbfj-wq/salcara-desktop/actions/runs/42485009684)。Node、companion、Go short、vet、race 通过。
- Desktop 同源 Windows 构建：[42485014079](https://github.com/dkdjndbfj-wq/salcara-desktop/actions/runs/42485014079)。Windows 原生测试、vet、打包、隔离用户安装/重装/降级保护/卸载、Ed25519 更新签名及完整产物校验通过。Windows 2025 隔离终端模块加载修复包含于该源码。
- Desktop 独立同源复核与草稿生成：[42486189825](https://github.com/dkdjndbfj-wq/salcara-desktop/actions/runs/42486189825)。校验 Actions 归档摘要、签名、USTAR 约束及便携包完整性。
- Hub 双架构发布：[42483662195](https://github.com/dkdjndbfj-wq/salcara-hub-plugin/actions/runs/42483662195)。完整 Go/vet/race、部署和前端回归、OCI 内容、provenance/SBOM、双架构导出及 amd64 非 root 容器烟测通过，然后复制同一归档到 GHCR。
- Hub 同源 Docker 门禁：[42483667446](https://github.com/dkdjndbfj-wq/salcara-hub-plugin/actions/runs/42483667446)通过。本机额外构建曾在 race 默认 10 分钟预算达到超时，未计作通过；正式 Runner 的完整门禁结果为成功。
- 公开发行后独立验收：[42487299752](https://github.com/dkdjndbfj-wq/salcara-desktop/actions/runs/42487299752)。无凭据匿名下载全部 18 个发行文件，核对精确发行标签、全部长度和 SHA-256，重新验证 Android ZIP/52 个原生库、Desktop Ed25519/USTAR/便携包、Hub 发行签名和双架构二进制。GHCR 匿名索引摘要相符。验证脚本仅在独立 verification 分支，未合入产品代码。

## 主要产物摘要

| 文件 | 字节数 | SHA-256 |
| --- | ---: | --- |
| salcara-image-android-v1.8.3.apk | 117227282 | 52ad02df9a45f3e66aa1d626c9be1a0557acf924ec11a948ec6b7f3507328465 |
| Salcara-Desktop-1.6.5-win32-x64-setup.exe | 152303145 | 06ed78dae73f80266b76551a86ff00d871073fa833058bb6b46c84772f126c2c |
| Salcara-Bridge-1.6.5-win32-x64.zip | 220472785 | 768ce2cf155a13daac45b4eccc2d28a1d2f214ee941df502bb2510332c7b2d0b |
| Salcara-Bridge-1.6.5-win32-x64.tar.gz | 214186007 | 58e7313a158bce65ca849ef3ff5f7e56bf2ea0760115d2e052b5dcbda9409387 |
| salcara-hub_0.5.3_linux_amd64 | 15122556 | 8ae390a8b9991f8c59eb9011fb88a7a839e335a0abf6b8c9e933bc6e51529905 |
| salcara-hub_0.5.3_linux_arm64 | 14155900 | 1c6921880f992a97c6de0b97dbee510fa65ba50b85071f726160c7ec39737faa |

Hub 镜像：ghcr.io/dkdjndbfj-wq/salcara-hub-standalone:0.5.3；OCI 索引 sha256:b930e8b7d5545be57c694952f54e15e7eec1f3bb61103b02cacf2dea9dfe91f7。amd64 和 arm64 均在索引中，同时保留构建证明与 SBOM。

## 更新入口与配套版本

Android updates 分支 latest.json 已在正式发行后生成，raw.githubusercontent.com 与 jsDelivr CDN 均回读到 1.8.3，APK 摘要一致。Hub main/updates/standalone-account.json 已更新到同一公开发行的 0.5.3 签名 envelope，原发布身份不变；公开 raw 内容与已验证 envelope 逐字节一致。旧 updates/standalone.json 保持不变。Desktop latest release 返回 v1.6.5，latest.json 与 latest.json.sig 独立复核通过。

Hub 内嵌公开配套版本为 Android 1.8.3、Desktop/Bridge 1.6.5。版本检查通过，既有隔离预览对首页/状态页的亮色、暗色及窄屏检查完成；未将该预览描述为用户线上服务器。没有提高最低支持版本，没有启用强制更新或分批控制，没有重新签发或替换既有发布密钥。

## 验收边界

- Windows 包未使用 Authenticode 证书签名；更新包的既有 Ed25519 签名已验证。
- arm64 为交叉构建、OCI 与文件校验，未宣称 arm64 实机容器烟测。
- 本次不包含用户 VPS 部署、实体手机公网全链路、真实收费 API、FCM 推送送达、iOS 签名分发或 macOS/Linux 桌面安装包。
- Hermes 历史会话通常固定原 API/key/root/model，切换后需要新会话；DeepSeek/Hermes Bridge 历史视图目前最多最近 400 个事件；Claude Bridge 重启后不自动接管旧进程会话。详细使用边界见三端发行说明。
