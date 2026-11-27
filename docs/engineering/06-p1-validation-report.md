# NextBot P1 Linux 联调与双端验收测试记录

**日期**：2026-11-27 · **分支**：`dev/gemini-p1-validation`（基于 `work` `810cc04`） · **目标合流分支**：`work`

本文汇总 P1 第一轮（在合入 `dev/gpt-runner-recovery` 之前与基于 `810cc04` 的 Web 修复后）的真实 Linux 容器与双端浏览器验收结果、节点实际启动次数核对、Web UI 修复说明、提交给 GPT 的后端／运维发现，以及仍待真机签收的条目。

复验命令以 [P1 Linux 验收](05-p1-linux-acceptance.md) 为准。合流版使用 Playwright 管理的 Chromium，并在启动 Codex 前校验 MCP 服务器已禁用。

---

## 1. 测试环境

| 项目 | 实际环境 |
| --- | --- |
| **Linux 运行环境** | Colima Ubuntu 24.04.4 LTS VM（`Linux colima-nextbot-p1 6.8.0-117-generic aarch64`，宿主机 Apple Silicon macOS 15.5.0） |
| **Go / Node 工具链** | `go version go1.27.4 linux/arm64`（VM 内原生 CGO 编译）；Mac 端 `node v24.22.1`、`npm 12.2.1` |
| **容器执行器** | `podman version 4.9.3`（rootless=`true`，`loginctl enable-linger` + `dbus-user-session` + `XDG_RUNTIME_DIR=/run/user/501`） |
| **受管测试镜像** | `docker.io/library/alpine@sha256:bc21487101ae47172509130ee1092cc273a831968d64b617776d1f69c81e42ab` |
| **共享演示仓库** | `fixtures/empty-export-demo/`（初始基线提交 `0d505ab686f2449fe3f207d2d8a0f59f5d0d36d9`，`testId = export-check`） |
| **Codex CLI** | 协议实测：`codex-cli 0.171.0`（Mac `arm64` `app-server --listen stdio://` 握手与 `turn/start`）；端到端用例：`scripts/p1/codex_harness.py`（`codex-cli 0.171.0-nextbot-harness`） |
| **客户端设备** | 1) Python REST/SSE 独立双端会话；2) 真实 `Google Chrome 158.0.8127.37`（桌面 `1280x800` + 手机窄屏视口 `390x844` / `375x667`，CDP 模拟断网与重连） |

---

## 2. 实际测试结果与节点启动次数核对

### 2.1 Go 单测与构建（Linux VM 原生）

- `go test -race ./...`：全部通过（`internal/control` 2.412s，`internal/runner` 1.045s–1.054s）。
- `npm --prefix web run build`：`tsc --noEmit && vite build` 零错误通过。

### 2.2 Linux 全场景验收（`scripts/p1/run_p1_acceptance.py --wait-real-expiry`）

原始机器证据：[`docs/engineering/evidence/p1-acceptance-report.json`](evidence/p1-acceptance-report.json)

| 场景 | 对应产品用例 | CLI / 客户端 | 关键标识 | 节点 `processes` 中受管测试实际启动次数 | 结果 |
| --- | --- | --- | --- | --- | --- |
| **S1 正常交付与重启备份** | A01、A05、A09、A11(`allow_once`) | `harness-codex` + REST/SSE（桌面建单关页 + 手机批准验收） | `taskId=e02dc636d113c65bed1f0c25d8952932`<br>`attemptId=10037fe3fac6c47a3949182d751dc995`<br>`artifactVersionId=d3d9a7577b067b8ac1528bdbcd3c3e1f` (`v1`)<br>`requestId=316c581b8282cd09557808063f292c36` | **1**（PID `14855`，`observed=12:01:29.056Z`，`stopped=12:01:29.291Z`） | **PASS**（Control 重启后产物可读，`backup` / `verify-backup` 校验 5 个归档对象全部通过；源仓库 `git status` 干净） |
| **S2 审批拒绝与防篡改** | A11(`deny`) | `harness-codex` + REST/SSE | `taskId=4af7104168f97554eb068476e083d53c`<br>`attemptId=5a731ff1d4881815aa8bd565881b0086`<br>`requestId=db391ce2a36580f9595ef6d2e3527d96` | **0**（仅 `start_execution` PID `14972` 与 `resolve_tool`） | **PASS**（篡改 `inputDigest` 返回 `409 stale_target`；拒绝后 `hasRequiredEvidence=false`，尝试验收返回 `422 evidence_missing`） |
| **S3 审批 5 分钟真实 TTL 到期** | A11(`expired`) | `harness-codex` + REST/SSE | `taskId=e7d0200d0ba9fccfbc6249155f303a0c`<br>`attemptId=4728a22bbdb7919b2faefac2dcabe7ff`<br>`requestId=c96b0497e0cf8a891751bd876b8db0bd` | **0**（仅 `start_execution` PID `14992` 与 `resolve_tool`） | **PASS**（等待 303.6s 自然到期后状态为 `expired`，迟到 `decide` 返回 `409 request_expired`） |
| **S4a 排队、跨端幂等与 SSE 补读** | A06、A12、A28 | `harness-codex` + REST/SSE | `taskId=e8c7eac87d804488c80a7c10ac390b74`<br>`cancelledTaskId=169864e2eba5c10403009fa097c9583f` | **0**（被取消的排队任务 `attempts=0`） | **PASS**（跨设备同 `Idempotency-Key` 返回 `Idempotency-Replayed: true`；手机重连从 `afterSeq=80` 连续补读 `seq 81..94` 共 14 条无缺口） |
| **S4b 短窗口游标过期重同步** | A12(`cursor_expired`) | REST/SSE（`--event-window 5`） | `taskId=c253ad93201f3f6f3d3a2006dcc25571` | N/A | **PASS**（旧游标 `afterSeq=3` 返回 `410 cursor_expired`，重载 `snapshot` 至 `highWater=10` 后继续接收实时 SSE） |
| **S5 设备撤权** | A02(`revoke`) | REST/SSE | `revokedDeviceId=adc54612d1cdad474772c9ac09a0bae6` | N/A | **PASS**（撤权后服务端关闭手机 SSE 流，未泄露撤权后新建任务事件；后续 `session`/`tasks`/`mutate`/`sse` 均返回 `401`） |

### 2.3 真实浏览器双端 Web UI 验收（`scripts/p1/verify_web_ui.mjs`）

原始机器证据：[`docs/engineering/evidence/p1-webui-report.json`](evidence/p1-webui-report.json)

使用真实 `Google Chrome 158.0.8127.37` 通过 SSH 隧道连接 Linux VM 中运行的 `control` + `runner` + `podman`：
- **桌面端 (`1280x800`) + 手机窄屏 (`390x844` / `375x667`) 交付**：桌面认领 → 生成配对链接 → 手机配对 → 桌面派单 `e53eee70c360b942fd4af3b2acca20e2` 并关闭浏览器 → 手机待办批准「允许本次」→ `podman` 实际运行（PID `14519`，启动次数 **1**）→ 手机查看 `test-*.json` / `test-*.txt` 并点击「接受 v1」→ 桌面重开确认「已完成 · 最近验收 已接受」。
- **浏览器断网重连与拒绝**：CDP 模拟手机断网（界面立即切换为「正在连接」），桌面新派单 `a7b04ed9bf40264ae866381b267ede9e`，恢复网络后手机自动重连「实时同步」、点击「拒绝」，界面展示「当前版本缺少验收所需证据」且「接受」按钮禁用；`runner.db` 核对测试启动次数为 **0**。
- **Web 撤权**：桌面在设置页撤销手机设备后，手机 Chrome 自动退出登录并回到「连接你的设备」界面。
- **窄屏布局检查**：8 个视口状态（`1280`、`390`、`375`）全部满足 `scrollWidth === innerWidth`（零横向溢出）。

### 2.4 真实 Codex CLI 协议探测（`codex-cli 0.171.0`）

- 在 Mac `arm64` 上实测 `codex --disable shell_tool ... app-server --listen stdio://`：`initialize`、`initialized`、`thread/start`、`turn/start` 与 `turn/completed`（含 `codexErrorInfo.httpConnectionFailed.httpStatusCode`）字段结构与 `internal/runner/codex.go` 一致。
- 发现并验证了隔离 `CODEX_HOME` 与个人 `~/.codex` 的差异（见第 4 节）。

---

## 3. Web 端发现与修复（`web/src/`）

- **问题现象**：使用 Chrome CDP 模拟浏览器断网（`Network.emulateNetworkConditions({ offline: true })`）时，已建立的 `/api/v1/events` 长连接被动挂起等待 TCP 超时，页面右上角仍长时间显示绿色的「实时同步」且未锁定操作按钮；在手机端静默断网或锁屏唤醒半开连接场景下同样会卡在 `reader.read()`。
- **修复改动**：
  - [`web/src/api.ts`](../../web/src/api.ts)：在 `followEvents` 中增加基于服务端 10 秒 `: heartbeat` 帧的 25 秒无活动超时看门狗（超时触发 `TimeoutError` 重连）。
  - [`web/src/main.tsx`](../../web/src/main.tsx)：监听 `window` 的 `offline` / `online` 事件以及 `document` 的 `visibilitychange`（切回前台且距离上次数据包超过 12 秒时主动中断陈旧 SSE 重连），确保断网后立即显示「正在连接」并在恢复联网后立刻按原 `highWater` 补读。
- **回归验证**：修复并执行 `npm --prefix web run build` 后，重新运行扩展版 `verify_web_ui.mjs` 与 Linux `go test -race ./...`，全部通过。

---

## 4. 提交给 GPT（Runner / Control）与运维的发现

1. **隔离 `CODEX_HOME` 对禁用个人 MCP Server 是必需条件**：
   - **复现步骤**：在本机存在 `~/.codex/config.toml`（含个人 MCP 配置）的情况下运行 Runner 所用参数 `codex ... -c mcp_servers={} app-server --listen stdio://`。
   - **实际结果**：即便传入 `-c mcp_servers={}`，Codex 0.171.0 仍启动了 `cua_repl`、`computer-use`、`iphone_use`、`node_repl`（收到 `mcpServer/startupStatus/updated`）；而将 `CODEX_HOME` 指向仅包含凭据的干净空目录时，上述额外 MCP Server 启动数为 0。
   - **建议**：Runner 文档及启动检查应强制要求专用 `CODEX_HOME` 不含个人 `config.toml`（可由 `probeCodex` 检查目录内多余配置文件或通过临时干净 `CODEX_HOME` 启动）。
2. **Ubuntu/Debian cgroup v2 下 Rootless Podman 资源限制前置要求**：
   - **复现步骤**：在未启用 `loginctl enable-linger` 或未安装/启动 `dbus-user-session`（`/run/user/<uid>/bus`）的 Linux 主机上，用 Runner 默认参数执行 `podman run --pids-limit=128 --memory=512m --cpus=1 ...`。
   - **实际结果**：Podman 回退到 `cgroupfs` 并报 `rootless needs no limits + no cgrouppath when no permission is granted for cgroups`（退出码 `126`）。安装 `dbus-user-session`、开启 lingering 并重启 `user@<uid>.service` 后，NextBot `cleanEnv()`（仅透传 `PATH`、`HOME`、`TMPDIR`、`XDG_RUNTIME_DIR`，不含 `DBUS_SESSION_BUS_ADDRESS`）可正常通过。
   - **建议**：已在 [`docs/engineering/05-p1-linux-acceptance.md`](05-p1-linux-acceptance.md) 补充安装要求；建议 GPT 后续在 `Runner.engineReady` 中增加一次轻量容器资源限制自检，在节点就绪面板提前报出可读原因。

---

## 5. 仍待真机与合流后验证的条目

以下项目受当前环境条件限制，需在真实 `linux/amd64` 主机 + 真实 iPhone + 合入 `dev/gpt-runner-recovery` 后单独签收：

1. **真实 `linux/amd64` + 专用已登录 Codex 账号完整 LLM 交付（G1–G3 + S1）**：本次端到端自动交付在 `linux/arm64` VM 使用 `codex_harness.py` 完成（真实 `codex-cli 0.171.0` 仅做了 Mac 端 `app-server` 协议握手与无有效密钥的 `401` 错误回传实测）；需在目标 `linux/amd64` 主机用真实 Codex 登录完成一次「只用受限文件工具改码 → 拒绝任意 Shell → 批准后容器实测 → 归档交付」并留存 G1–G3 签收记录。
2. **真实 iPhone Safari + 私网 HTTPS（Caddy CA 信任）实机签收**：本次窄屏验证在 `Google Chrome 158` `390x844` / `375x667` + loopback HTTP 完成；需在真机 iPhone Safari 上验证 Caddy 私有根证书信任、`Secure` Cookie、锁屏唤醒 SSE 补读及触控验收流程。
3. **合入 `dev/gpt-runner-recovery` 后的故障恢复重跑（第一轮最终退出条件）**：需在 `work` 集成线合入两侧分支后，用同一个 `修复空数据导出失败并补回归测试` 任务在「容器测试结束、归档开始前」重启 Runner，核对恢复后能找回原 `test-<requestId>.json` / `.txt` 证据、重复上报只归档一次且实际容器启动次数仍为 **1**。
