# API 电脑登记与恢复

这是下一版源码协议说明，不表示当前线上站点已经启用。普通用户在 Desktop
选择已保存的 API 后连接；API 校验成功只登记电脑，手机控制仍需要一次明确扫码授权。
模型列表请求成功不能替代账户身份或远程使用资格验证。

## 站点启用条件

- Hub 使用完整、可写且持久保存的 DataDir，并已配置签名入口身份。
- Hub 的 station 来源策略明确允许完整 API base；指定 issuer 的
  `deviceEnrollment` 显式开启，并配置同源 HTTPS `enrollmentUrl`。
- 签发服务预先固定 Hub 的 audience、instance ID、公钥，路由使用真实
  `enrollmentVerifierUrl`，对应分组显式允许 `allowDeviceEnrollment`。
- 本批站点适配对象为 Salcara Sub2API 的真实 API Key 认证链，新增
  `/v1/salcara-hub/enrollment-identity`；旧 `/v1/salcara-hub/key` 保持原协议。
  未安装该适配的兼容 API 不能被宣传为支持自动登记。

具体配置与签名格式见 [签发服务说明](SOURCE-ISSUER-SERVICE-20261015.md#opt-in-device-enrollment-v1)。
所有新增权限默认关闭。Hub 公钥使用标准带填充 Base64；issuer 公钥和消息 envelope
使用无填充 Base64URL，不能混用。原始 API Key 仅到固定同源签发端，不能到 Hub、手机
或重定向目标；响应、日志与设备状态不包含 API Key 或外站原始主体。

## 响应丢失和失败后的恢复

Desktop 保留同 Hub pin、device ID、device secret、账号及配置版本。先用原设备凭证
查询 `/discovery/policy`：200 表示已登记；泛 401/403 不能说明“没有登记”。
只有用户主动重试，才能为同设备请求新 challenge；Hub 会等待在途登记落盘并检查
所有命名空间。已有身份返回 409 `enrollment_request_conflict`，停用用户保持 423
`user_disabled`，封禁拒绝。验证新鲜的同 pin 签名 challenge 后，Desktop 才能向
固定签发端重新申请授权。最终提交再次检查，不能覆盖并发登记的赢家。

登记完成后恢复连接不依赖 API Key，换 Key 也不新建电脑。网络/5xx 不自动重放未知
结果、不生成新 ID，也不改变手机绑定。旧式登记或 Bot 登记写盘失败后保留 pending
标记；后台保存和重启不能将它升级为成功。恢复可写存储后，用相同设备身份重试。
若 Bot 组写入失败后重启，设备的 pending 记录存在但组归属未保存，需要用原绑定包
重试恢复组归属；仅凭设备密钥不能把这个未完成状态确认成成功。

## 管理员的来源主体关联

外站 subject 不是 Hub 用户编号。管理员将某台 API 登记电脑关联到用户后，Hub
持久保存该 issuer/subject 的归属。以后同主体使用新 Key 或新 device ID，仍受该
用户的停用状态约束；清理最后一台电脑不会清除来源关联。冲突时拒绝单台静默迁移。

如确需释放来源关联，使用现有管理员 `POST /salcara-hub/_admin/v1/users`，包括：

```json
{
  "action": "release-source",
  "expected_revision": "从用户管理响应读取的当前 revision",
  "id": "目标 Hub 用户 ID",
  "source_ref": "该用户 enrollment_associations 中的精确不透明编号",
  "reason": "明确的管理原因（不含密钥）",
  "confirm": true
}
```

沿用管理员会话、同源和 CSRF 校验，以及操作审计。不能传设备编号代替来源编号，
也不能批量猜测其他用户的来源关联。此动作仅允许未来登记重新归属：现有电脑的用户、
停用/封禁状态和手机绑定不变。新映射也不会自动搬移旧电脑。删除用户不隐式释放来源。
管理页仅暴露不透明关联编号，不返回原始 subject 或 Key。

## 备份与范围

备份、迁移和恢复必须保留完整 DataDir，包括 devices、managed-users、Bot 组与
签名身份等文件。只拷邀请包、公钥或单个设备文件不足以恢复授权。设备记录中一并保存
可信来源、消费过的 request/challenge 与 pending 状态；Bot 先提交组再确认设备身份。
旧格式无新增字段时保持可读，畸形或重复的登记记录拒绝加载。

本批不包含发布、服务器部署、版本号变更、通知策略改造或设计稿落入项目。真实浏览器
渲染与手机实机仍需单独验收，合成服务测试不能替代这些验收。
