# 可视化沙箱工作台

可视化沙箱工作台把会话已绑定的 Docker、CubeSandbox 或 E2B 沙箱映射为统一的终端、文件和预览界面。浏览器始终只提供会话 ID 和相对路径，不能指定提供商沙箱 ID。

## 架构

```text
ChatHeader
  └─ SandboxWorkbenchDrawer
       ├─ 终端：POST /sessions/:id/sandbox/terminal/exec
       ├─ 文件：GET/POST/DELETE /sessions/:id/sandbox/files
       └─ 预览：HTML / PPTX / XLSX / PDF / 图片 / 文本
                        │
                        ▼
SandboxWorkbenchService
  ├─ SessionService.GetOwnedSession（用户和租户隔离）
  ├─ sessions.sandbox_config_id（服务端绑定）
  ├─ TenantSandboxResolver（Docker / Cube / E2B）
  ├─ SessionShellExecutor
  └─ SessionFileStore
```

## API

| 方法 | 路径 | 用途 |
|---|---|---|
| `GET` | `/api/v1/sessions/:id/sandbox/workbench` | 返回后端类型和可用能力 |
| `GET` | `/api/v1/sessions/:id/sandbox/files?path=` | 列出产物目录 |
| `GET` | `/api/v1/sessions/:id/sandbox/files/content?path=` | 预览或下载文件 |
| `POST` | `/api/v1/sessions/:id/sandbox/files` | multipart 上传 |
| `POST` | `/api/v1/sessions/:id/sandbox/files/rename` | 重命名 |
| `DELETE` | `/api/v1/sessions/:id/sandbox/files?path=` | 删除 |
| `POST` | `/api/v1/sessions/:id/sandbox/terminal/exec` | 执行终端命令 |

## 边界

- 文件 API 仅允许 `/workspace/output` 下的相对路径。
- 绝对路径、`..` 穿越、反斜杠和经符号链接解析后越界的路径都在服务端拒绝。
- 所有读写和命令执行均先调用 `GetOwnedSession`，空间管理员也不能修改其他主体的会话沙箱。
- HTML 使用不含 `allow-same-origin` 的 sandbox iframe；服务端内联响应另附加 CSP。
- 终端超时上限为 300 秒，用户中断会取消 HTTP 请求上下文并传递到沙箱适配器。
- 终端命令和文件变更会写入现有的租户审计日志。

## 前端预览

- 演示文稿：`@vue-office/pptx`。
- 表格：SheetJS 解析后渲染受控表格，不执行工作簿中的脚本。
- 网页：Blob URL + sandbox iframe，无同源权限。
- PDF、图片和文本：浏览器内置预览。

## 演示文稿 Skill

`skills/preloaded/presentation-generator` 从标准输入读取 JSON，并把可编辑 PPTX 和自包含 HTML 预览写到 `WEKNORA_SKILL_OUTPUT_DIR`。

## 验证命令

```powershell
go test ./internal/application/service -run '^TestSandboxWorkbench|^TestCleanArtifact' -count=1
go test ./internal/handler/session ./internal/router -count=1

cd frontend
npm run type-check
npm test
```
