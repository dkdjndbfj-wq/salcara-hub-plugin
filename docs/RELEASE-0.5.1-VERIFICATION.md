# Hub 0.5.1 发布验收

- 被测源码：`8ce50d6057cf62b11efc7e3126c689038d1d8906`。
- 正式构建/镜像发布：`37472251139`；同提交常规 Docker CI：`37472243060`，均成功。
- 安装器 14 项安全回归、admin UI、release staging、Go unit/vet/race、非 root 只读容器 smoke 成功；两份 ELF 架构和四份 Hub/launcher 哈希已本地核对。
- 使用仓库外原发布者私钥离线签名，公钥身份不变。独立公钥核验通过后公开 [standalone-v0.5.1](https://github.com/dkdjndbfj-wq/salcara-hub-plugin/releases/tag/standalone-v0.5.1)。再次下载六份公开资产并复验签名和两份 Hub 实际文件。
- 镜像匿名读取 OCI index 与两平台 config 成功：amd64/arm64、版本 0.5.1、非 root 用户 65532:65532。
- 镜像 digest：`sha256:04f878e47492768934629131491146238d92ffb1ab661299ca3b751b7b46a7f6`。

正式更新指针使用这次 Release 的同一 `standalone-account.json`，仅在公开下载复验后更新；旧 `updates/standalone.json` 不变。没有轮换公钥、触碰生产服务器、删除数据卷或更新 Sub2API。

额外发布修复：首次安装的 Release/health 正则改为从版本变量推导，并新增旧标签拒绝回归；修复版本上升后错误拒绝初始化。部署教程分别给出 0.5.x 修复更新和 0.4.x 认证迁移，已有卷禁止重跑首次初始化。

签名私钥、真实管理密钥、API Key 与服务器凭证未提交。arm64 是交叉构建/镜像 manifest 核验，不是 arm64 实机运行测试；真机手机、双公网 Hub 和弱网端到端仍需验收。
