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
| `interactive` | 支持交互式 PTY 终端（已实现 Docker / E2B 适配器） |
| `files` | 支持产物文件管理 |

交互式 PTY 由可选能力接口 `sandbox.SessionTerminalProvider` 提供；尚未实现流式适配的 Cube 后端保持 nil，前端降级为命令模式。E2B 使用现有 SDK 的 `Pty.Create/SendInput/Resize/Kill`，不是将聚合命令输出伪装成流。

E2B PTY 当前接受默认 `bash -l` 登录 shell，拒绝其他 argv；操作账号固定为 `1000`。长连接使用终端租约，首次响应另设 10 秒时限；普通控制、文件、输入和尺寸请求保留 HTTP 超时。关闭或取消时另用短时独立上下文清理带随机终端标记的进程，并调用 PTY Kill；清理失败会返回错误。模板须具备 bash、sh、grep 及 `/proc`，并为执行账号准备 `/workspace`。

上述说明区分“适配器实现”与“真实后端验收”：协议夹具测试通过不表示真实 E2B 环境已通过。真实环境测试命令如下，缺少密钥或模板时直接报错：

```bash
# 从本地配置注入 E2B_INTEGRATION_API_KEY、E2B_INTEGRATION_TEMPLATE；
# 自建环境另配 API_URL、SANDBOX_DOMAIN、PROXY_URL（同 E2B conformance）。
go test -tags='workbench_integration e2b_integration' ./internal/sandbox \
  -run '^TestE2BTerminalRealIntegration$' -count=1 -v -timeout=5m
```

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
- 终端以 `DefaultSandboxExecUser` 运行（与模型执行同一账户契约）。Docker 的固定启动段先报告内核 PID、POSIX session ID 与启动时间，再执行 shell；标识头不发送到浏览器。终端没有一次性命令的 timeout 包装，生命周期由租约决定。
- 单文件上传上限 20 MiB；终端命令超时上限 300 秒（命令模式）。
- 每会话最多 2 个并发交互终端；终端租约 30 分钟，到期 PTY 随上下文终止，浏览器收到 `reason=lease_expired`。
- Docker 关闭 attach 连接本身不会终止 exec。适配器使用内核身份与随机继承标记核对终端进程范围，显式清理进程与后台作业；另一个终端不受影响。清理错误进入关闭审计的 `cleanup_error` 字段，不伪装成成功。
- WebSocket 在进程退出、租约结束、浏览器断开或异常帧到达时结束，不再等待下一次键盘输入。退出事件与单条关闭审计记录同一原因和实际退出码；后端故障不再误标为租约到期。关闭审计使用保留租户/操作者信息的独立短时上下文，避免断开连接后丢失记录。
- 交互终端期间每 4 分钟经包装命令刷新一次 Docker 空闲回收的活动标记，避免挂着终端的容器被判定空闲回收。
- HTML 预览使用不含 `allow-same-origin` 的 sandbox iframe；服务端内联响应另附加 CSP。
- 审计：`sandbox.terminal_opened` / `sandbox.terminal_closed`（含原因与退出码）、`sandbox.terminal_command`（命令模式记录完整命令与结果；交互模式由输入流重建命令行，Ctrl-C 记为 `^C` 并标记 interrupted）、`sandbox.file_written` / `sandbox.file_renamed` / `sandbox.file_deleted`。

## 前端预览

- 演示文稿：`@vue-office/pptx`；Skill 同时产出自包含 HTML 逐页放映。
- 表格：SheetJS 解析后渲染受控表格，不执行工作簿中的脚本。
- 网页：Blob URL + `sandbox="allow-scripts"` iframe，无同源权限。下载响应的 CSP 不会随 Blob 自动继承，因此前端在任何产物内容之前插入文档级 CSP，禁止 fetch、外链脚本、外链图片、子框架与表单提交，仅保留自包含内容所需的内联脚本、样式和 data/blob 图片。此处不将 iframe 属性检查等同于实际浏览器隔离验证。
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

# 真实前端组件 + 模拟 HTTP/WebSocket 的浏览器测试（不是实际沙箱集成）
npx playwright test --config playwright.final.config.ts
```

该浏览器套件增加恶意 HTML 验证：尝试读取父页面、Cookie、本地存储，移除策略 meta 后再发送 fetch/图片请求；断言读取均被拒绝且网络请求计数为零。原实现对此测试失败，修复后通过。测试报告写入 `frontend/e2e-artifacts/final-results.json`。

Windows 上如开发服务器自动清理受进程权限影响，可在一个终端单独运行 `npm run dev -- --host 127.0.0.1 --port 8137 --strictPort`，再在另一个 PowerShell 终端设置 `$env:WEKNORA_E2E_EXTERNAL_SERVER='1'` 后运行上面的 Playwright 命令。测试结束后关闭该开发服务器。`WEKNORA_E2E_BASE_URL` 可覆盖外部服务器地址。

### 真实 Docker 终端验收

以下测试使用实际 Docker Engine 和真实 shell，不拦截后端接口。测试自行创建及清理无网络、64 MiB 内存、0.5 CPU 和 64 PID 上限的容器，不连接已有业务容器。测试镜像仅用于终端生命周期验证，不替代生产 Skill 镜像。

```bash
docker build --pull=false -f docker/Dockerfile.workbench-test -t weknora-workbench-test:20260907 docker
WORKBENCH_INTEGRATION_IMAGE=weknora-workbench-test:20260907 \
  go test -tags workbench_integration ./internal/sandbox \
  -run 'TestDockerTerminal.*Integration' -count=1 -v -timeout 90s
```

覆盖主动关闭、取消、时长到期、脱离原进程组的后台作业、shell 正常退出后的子进程清理，以及 UID 1000、实时输出、Ctrl-C 返回 130、37×111 终端尺寸和并行终端互不影响。CPU/内存超限终止和跨租户授权验收需分别运行对应测试，不能用此套件替代。

## 已知限制

- Docker 与 E2B 已实现交互式 PTY 适配；E2B 仍须用真实接入环境完成验收，不能以协议模拟测试代替。Cube 当前仍走命令模式。
- 交互模式下审计的是"用户键入的命令行"（含退格修正前的最终形态），不是 shell 实际展开后的执行体；命令模式的审计为完整聚合结果。
- 预览派生文件（如 PPTX 转 HTML）由 Skill 在沙箱内生成；沙箱回收后原文件不可用，界面提示重新生成。
