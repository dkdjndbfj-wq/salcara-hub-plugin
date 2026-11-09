# 配对电脑的项目目录命令

`workspace.projects` 使用现有工作区权限，不要求模型来源授权。Hub 只验证协议、配对身份和目标电脑；Bridge 通过 `workspace.projects.v1` 声明能力，核验本机 Agent、明确允许目录和最终文件句柄。客户端需先判断 Bridge 能力，不能把 Hub 接受命令当作电脑已支持该功能。

```json
{"type":"workspace.projects","controlSurface":"cli","tool":"codex","workspaceMode":"code","action":"list","cwd":"","path":""}
```

所有字段均为字符串，不接受重复字段、额外字段或 `null`。必填字段如上；`workspaceMode` 为 `code` / `work`，`action` 为 `list` / `select` / `mkdir`。`tool` 保留客户端指定值，由目标电脑核验对应 Agent 的安装及读取能力，不自动改成另一个 Agent。

- 空 `cwd/path` 的 `list` 读取电脑明确授权的根目录。无授权返回空列表，不回退 home 或磁盘。
- 非空 `cwd` 是所选精确授权根；`path` 是其中的相对目录。`list` 可带字符串 `cursor`，Bridge 按原有分页返回目录。
- `select` 验证现有目录并返回 `{tool,workspaceMode,cwd,path,project:{path,name}}`；Bridge 保存该精确项目授权后，手机另行发送显式 `session.start`。
- `mkdir` 额外接受单段 `name`，在所选相对目录中创建文件夹并返回同样的 project。不得自动重试结果不确定的创建；先刷新确认同名目录。

Hub 拒绝穿越、绝对相对路径、分隔符混淆、隐藏/系统目录名、控制字符和旁路字段。它不会在服务器上解析 Windows/Linux 实际路径或授权磁盘。Bridge 仍须按 `cwd` 精确选择本机授权根，验证目录存在、Agent 能力、符号链接/reparse、最终句柄以及操作前后的授权和取消状态。未知 Agent、未授权目录、已撤销配对均不得回退到其他 Agent/电脑/目录。

工作区权限在 `open`、`restricted`、`disabled` 模型来源模式下保持相同的配对边界，不增加模型执行权限。`requestId` / `receiptOnly` 持久化路径返回 `workspace_receipt_unsupported`，目录内容只通过短期请求回复传递。Bridge 的错误文本（如 `project_not_authorized`、`project_unavailable`、`project_exists`）按现有工作区回复传递，Hub 不根据错误重发。

Hub 回归位于 `internal/hub/workspace_projects_policy_test.go`，覆盖 schema、路径、配对、目标电脑、迟到回复、模型权限独立和不落内容回执；真实文件系统与手机切换状态由配套 Bridge/手机测试验证。
