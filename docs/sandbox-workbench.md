# 可视化沙箱工作台

可视化沙箱工作台把会话已绑定的 Docker、CubeSandbox 或 E2B 沙箱映射为统一的终端、文件和预览界面。浏览器始终只提供会话 ID 和相对路径，不能指定提供商沙箱 ID。

## 架构

```text
ChatHeader
  └─ SandboxWorkbenchDrawer
       ├─ 终端：交互式 PTY（WebSocket）或命令模式（REST，按能力降级）
       ├─ 文件：目录浏览 / 上传 / 下载 / 重命名 / 删除
       └─ 预览：HTML / PPTX / XLSX / PDF / 图片 / 文本
                        │
                        ▼
SandboxWorkbenchService
  ├─ SessionService.GetOwnedSession（用户和租户隔离）
  ├─ sessions.sandbox_config_id（服务端绑定）
  ├─ TenantSandboxResolver（Docker / Cube / E2B）
  ├─ SessionShellExecutor（命令模式、保活）
  ├─ SessionTerminalProvider（交互式 PTY，能力协商）
  └─ SessionFileStore
                        │
                        ▼
DockerRemoteClient.ExecStream（TTY exec + ExecResize）
```

## 能力协商

`GET /workbench` 返回后端与能力，前端按能力渲染而不是按后端名分支：

| 字段 | 含义 |
|---|---|
| `backend` | 会话绑定的后端类型（docker / cube / e2b） |
| `terminal` | 支持命令模式（一次性执行并返回聚合输出） |
| `interactive` | 支持交互式 PTY 终端（当前为 Docker） |
| `files` | 支持产物文件管理 |

交互式 PTY 由可选能力接口 `sandbox.SessionTerminalProvider` 提供；不支持流式传输的后端（Cube/E2B 的 envd HTTP exec）保持 nil，前端自动降级为命令模式，不伪装成交互终端。

## 交互式终端 WebSocket 协议

`GET /api/v1/sessions/:id/sandbox/terminal/ws?cols=120&rows=36&tenant_id=<可选>`

| 方向 | 帧类型 | 载荷 | 语义 |
|---|---|---|---|
| C→S | 二进制 | 原始终端字节 | 键盘输入（含 Ctrl-C 的 0x03） |
| C→S | 文本 | `{"type":"resize","cols":120,"rows":36}` | xterm FitAddon 变化，50ms 合并 |
| C→S | 文本 | `{"type":"ping","seq":N}` | 保活（25s 间隔；服务端 75s 无帧回收） |
| S→C | 二进制 | 原始 PTY 字节 | 终端输出，直接喂给 xterm.js |
| S→C | 文本 | `{"type":"ready","terminal_id":…,"backend":…}` | 终端就绪 |
| S→C | 文本 | `{"type":"exit","code":N,"reason":…}` | 进程退出 / 租约到期 / 连接断开 |
| S→C | 文本 | `{"type":"error","error":…,"message":…}` | 授权、上限或后端错误 |
| S→C | 文本 | `{"type":"pong","seq":N}` | ping 应答 |

### 鉴权

浏览器无法在 WebSocket 握手上携带自定义头。JWT 以 `bearer.<token>` WebSocket 子协议发送，认证中间件把它提升为常规 `Authorization` 头（`internal/middleware/auth.go` 的 `websocketBearerProtocolToken`）；跨空间切换的 `X-Tenant-ID` 同理由 `tenant_id` 查询参数提升（`promoteWebSocketQueryHeaders`）。token 不进入 URL，因此不会出现在访问日志里。WebSocket 的 Origin 校验放行同源与已认证的 bearer 握手（跨站页面拿不到 token）。

## REST API

| 方法 | 路径 | 用途 |
|---|---|---|
| `GET` | `/api/v1/sessions/:id/sandbox/workbench` | 返回后端类型和可用能力 |
| `GET` | `/api/v1/sessions/:id/sandbox/files?path=` | 列出产物目录（含子目录） |
| `GET` | `/api/v1/sessions/:id/sandbox/files/content?path=` | 预览或下载文件 |
| `POST` | `/api/v1/sessions/:id/sandbox/files` | multipart 上传 |
| `POST` | `/api/v1/sessions/:id/sandbox/files/rename` | 重命名 |
| `DELETE` | `/api/v1/sessions/:id/sandbox/files?path=` | 删除 |
| `POST` | `/api/v1/sessions/:id/sandbox/terminal/exec` | 命令模式执行 |

## 边界与安全

- 文件 API 仅允许 `/workspace/output` 下的相对路径；绝对路径、反斜杠、空字节、`..` 穿越、经符号链接解析后越界的路径都在服务端拒绝（先词法清洗，再在沙箱内 `realpath -m` 复核）。
- URL 编码的遍历在 HTTP 层解码后落入同一套检查；双重编码只是字面文件名，无法越界。
- 所有读写、命令执行、终端打开都先走 `GetOwnedSession`，空间管理员也不能操作其他主体的会话沙箱。
- 终端以 `DefaultSandboxExecUser` 运行（与模型执行同一账户契约），PTY exec 不经过超时包装命令——终端生命周期由租约决定。
- 单文件上传上限 20 MiB；终端命令超时上限 300 秒（命令模式）。
- 每会话最多 2 个并发交互终端；终端租约 30 分钟，到期 PTY 随上下文终止，浏览器收到 `reason=lease_expired`。
- 交互终端期间每 4 分钟经包装命令刷新一次 Docker 空闲回收的活动标记，避免挂着终端的容器被判定空闲回收。
- HTML 预览使用不含 `allow-same-origin` 的 sandbox iframe；服务端内联响应另附加 CSP。
- 审计：`sandbox.terminal_opened` / `sandbox.terminal_closed`（含原因与退出码）、`sandbox.terminal_command`（命令模式记录完整命令与结果；交互模式由输入流重建命令行，Ctrl-C 记为 `^C` 并标记 interrupted）、`sandbox.file_written` / `sandbox.file_renamed` / `sandbox.file_deleted`。

## 前端预览

- 演示文稿：`@vue-office/pptx`；Skill 同时产出自包含 HTML 逐页放映。
- 表格：SheetJS 解析后渲染受控表格，不执行工作簿中的脚本。
- 网页：Blob URL + sandbox iframe，无同源权限。
- PDF、图片和文本：浏览器内置预览。

## 演示文稿 Skill

`skills/preloaded/presentation-generator` 从标准输入读取 JSON，并把可编辑 PPTX 和自包含 HTML 预览写到 `WEKNORA_SKILL_OUTPUT_DIR`。

## 验证命令

```bash
# 交互终端（fake 引擎，无需 Docker）
go test ./internal/sandbox -run 'TestExecStream' -count=1 -v
# 工作台服务（路径安全 / 能力协商 / 终端上限与审计）
go test ./internal/application/service -run 'SandboxWorkbench|CleanArtifact' -count=1 -v
# WS handler 全链路（httptest + gorilla 客户端）与中间件子协议桥接
go test ./internal/handler/session -run 'TerminalWorkbench' -count=1 -v
go test ./internal/middleware -run 'TestBearerTokenFromWebSocketSubProtocol' -count=1 -v

cd frontend
npm run type-check
npm run build
```

## 已知限制

- 交互式 PTY 终端当前仅 Docker 后端；Cube/E2B 走命令模式，待 envd 流式传输就绪后经同一能力接口接入。
- 交互模式下审计的是"用户键入的命令行"（含退格修正前的最终形态），不是 shell 实际展开后的执行体；命令模式的审计为完整聚合结果。
- 预览派生文件（如 PPTX 转 HTML）由 Skill 在沙箱内生成；沙箱回收后原文件不可用，界面提示重新生成。
