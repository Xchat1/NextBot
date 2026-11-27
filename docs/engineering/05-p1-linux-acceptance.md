# NextBot P1 Linux 联调、安装与双端验收说明

本文档面向 `dev/gemini-p1-validation` 的 P1 单机双端联调与验收，配合 [P1 工程交付与验收计划](04-p1-delivery.md)、[API 与控制协议](02-api-protocol.md) 与 [持久化与恢复](03-data-recovery.md) 使用。

---

## 1. 共用演示任务与夹具

两路开发线（Runner 恢复与 Linux/双端验收）共用同一个演示任务与演示仓库夹具：

- **夹具目录**：[`fixtures/empty-export-demo/`](../../fixtures/empty-export-demo/README.md)
- **初始化脚本**：[`fixtures/empty-export-demo/init-demo-repo.sh`](../../fixtures/empty-export-demo/init-demo-repo.sh)
- **任务标题**：`修复空数据导出失败并补回归测试`
- **基线缺陷**：`src/export_report.sh` 对 `samples/normal-input.csv` 可正常导出，但对 `samples/empty-input.csv`（仅表头）与 `samples/blank-input.csv`（0 字节）返回退出码 `1` 并报错 `export failed: empty dataset`。
- **受管测试项**：`export-check`（`tests/export_check.sh`），运行于只读 `/workspace`、无网络 `--network=none`、不可执行 `/tmp` 与 `/output` 的一次性 rootless 容器内，校验正常导出、空数据导出与对照样本（`samples/expected-normal.csv`、`samples/expected-empty.csv`）一致，并自动运行新增的 `tests/test_*.sh` / `tests/*_regression.sh`。

### 建议的共享建单字段

| 字段 | 推荐值 |
| --- | --- |
| `title` | `修复空数据导出失败并补回归测试` |
| `goal` | `修复 src/export_report.sh 在空数据（samples/empty-input.csv 与 samples/blank-input.csv）时导出失败的问题，保持表头 id,region,amount 与普通输入导出格式不变，补充回归测试并运行受管测试 export-check。` |
| `scope` | `仅修改 src/export_report.sh 和 tests/ 目录下的测试脚本，不修改 samples/ 对照样本。` |
| `acceptanceCriteria` | `1) samples/normal-input.csv 输出与 samples/expected-normal.csv 一致；2) samples/empty-input.csv 与 samples/blank-input.csv 均退出码 0 且输出与 samples/expected-empty.csv 一致；3) 受管测试 export-check 返回退出码 0。` |
| `executionAgent` | `codex` |
| `reviewAgent` | 空（P1 直接人工验收） |
| `testId` | `export-check` |
| `requireTest` | `true` |

---

## 2. 干净 Linux 实例安装与启动

### 2.1 前置环境

> 本节的受管容器验收需要 Linux 主机或 Linux VM。Mac 桌面应用可运行本机文件任务，参见 [私网部署与维护](06-private-deployment.md)。

- **OS**：Linux（首选 `x86_64`；`aarch64` 如 Lima/Colima VM 需单独标注架构）
- **工具链**：Go 1.25+、GCC / libc6-dev（`mattn/go-sqlite3` CGO 编译）、Node.js 22.12+、Git、Python 3、SQLite CLI
- **容器执行器**（二选一，必须为 rootless 模式）：
  - **Rootless Podman**（推荐）：`podman info --format '{{.Host.Security.Rootless}}'` 输出 `true`
    - Ubuntu/Debian cgroup v2 主机启用资源限制（`--pids-limit`、`--memory`、`--cpus`）时，需安装 `dbus-user-session`、`uidmap`、`slirp4netns`，并确保运行 Runner 的系统账号开启 lingering 且存在 user systemd D-Bus session：
      ```sh
      sudo apt-get install -y podman dbus-user-session uidmap slirp4netns
      sudo loginctl enable-linger "$USER"
      sudo systemctl start "user@$(id -u).service"
      export XDG_RUNTIME_DIR="/run/user/$(id -u)"
      ```
    - 检查运行账号在 `/etc/subuid`、`/etc/subgid` 有独立的映射范围。缺少映射时，Podman 可能无法解包镜像。缺少用户 D-Bus 会话时，容器可能无法应用资源限制。
  - **Rootless Docker**：`docker info --format '{{json .SecurityOptions}}'` 包含 `rootless`
- **Codex CLI**：
  - 安装真实 `codex`（如 `@openai/codex`），并准备**独立的专用 `CODEX_HOME`**，不要复用包含个人 MCP Server 的 `~/.codex/config.toml`（避免加载本地自定义 `mcp_servers.*`）。

### 2.2 一键初始化干净实例

```sh
make build
./scripts/p1/setup-clean-instance.sh \
  --state-dir /tmp/nextbot-p1 \
  --engine podman \
  --image docker.io/library/alpine:3.21 \
  --origin https://nextbot.example.internal \
  --https \
  --force
```

脚本会完成：
1. 校验 rootless Podman/Docker，拉取并解析带 `@sha256:...` 摘要的镜像；
2. 生成干净独立的演示 Git 仓库 `/tmp/nextbot-p1/demo-repo`（记录 `BASE_COMMIT`）及 `/tmp/nextbot-p1/tests.json`；
3. 调用 `bin/nextbot project-add` 登记项目 `"报告服务"` 与 `export-check` 测试项；
4. 输出 `/tmp/nextbot-p1/instance.env`。

> 注意：`control.db`（WAL）、`runner.db` 与 `runner.sock` 必须放在原生 Linux 文件系统（如 `/var/lib/nextbot` 或 `/tmp/...`），不要放在宿主机通过 virtiofs/9p 挂载的共享目录上。

### 2.3 私网 HTTPS（桌面 + iPhone 真机验收）

1. 在 Linux 主机安装 Caddy，将 `https://nextbot.example.internal` 反向代理到 `127.0.0.1:8787`（禁用响应缓冲）：
   ```caddyfile
   nextbot.example.internal {
       tls internal
       reverse_proxy 127.0.0.1:8787 {
           flush_interval -1
       }
   }
   ```
2. 确保电脑和手机通过同一私网 / Tailscale / WireGuard 解析 `nextbot.example.internal` 到该 Linux 主机，并在电脑系统钥匙串与 iPhone（安装 Caddy root CA 描述文件并在「设置 → 通用 → 关于本机 → 证书信任设置」启用完全信任）信任证书。
3. 在两个终端分别启动 Control 与 Runner：
   ```sh
   # 终端 1：Control
   bin/nextbot control \
     --data /tmp/nextbot-p1/control \
     --socket /tmp/nextbot-p1/runner.sock \
     --listen 127.0.0.1:8787 \
     --origin https://nextbot.example.internal

   # 终端 2：Runner
   bin/nextbot runner \
     --data /tmp/nextbot-p1/runner \
     --socket /tmp/nextbot-p1/runner.sock \
     --codex-home /tmp/nextbot-p1/codex-home \
     --verified-codex-version "$(codex --version)" \
     --container-engine podman \
     --enable-managed-tests
   ```
4. 首次认领与手机配对：
   ```sh
   bin/nextbot claim --data /tmp/nextbot-p1/control --origin https://nextbot.example.internal
   ```
   - 在桌面浏览器打开输出的一次性 `#claim=...` 链接完成所有者绑定；
   - 在「设置 → 可信设备 → 配对设备」生成一次性 `#pair=...` 链接，在手机 Safari 打开完成配对；
   - 核对「设置 → 执行节点」显示在线、`Codex` 为 `执行及受管测试`。

---

## 3. 可重复执行的 P1 验收脚本

[`scripts/p1/run_p1_acceptance.py`](../../scripts/p1/run_p1_acceptance.py) 基于现有 REST/SSE 接口驱动完整 P1 验收，并直接核对节点 `runner.db` 进程记录与源仓库隔离状态：

### 快速验证（含正常交付、拒绝、排队幂等、SSE 断线补读、410 快照重同步、设备撤权、重启与备份校验）

```sh
python3 scripts/p1/run_p1_acceptance.py \
  --state-dir /tmp/nextbot-p1-run \
  --binary bin/nextbot \
  --engine podman \
  --cli-mode harness
```

### 完整 5 分钟真实 TTL 审批过期验收（追加 `--wait-real-expiry`）

```sh
python3 scripts/p1/run_p1_acceptance.py \
  --state-dir /tmp/nextbot-p1-full \
  --binary bin/nextbot \
  --engine podman \
  --cli-mode harness \
  --wait-real-expiry
```

### 使用真实 Codex CLI 执行主交付（`--cli-mode real-codex`）

```sh
python3 scripts/p1/run_p1_acceptance.py \
  --state-dir /tmp/nextbot-p1-realcli \
  --binary bin/nextbot \
  --engine podman \
  --cli-mode real-codex \
  --codex-home /path/to/dedicated/codex-home
```

> 说明：`codex_harness.py` 实现与 `internal/runner/codex.go` 一致的 stdio `app-server` JSON-RPC 协议，用于无模型波动的确定性故障与回归测试；正式签收时需明确区分哪些用例使用真实 `codex` CLI 与真实 iPhone Safari，哪些使用辅助夹具。

### 浏览器交接

在已登记演示仓库、尚未认领的 P1 实例上使用新签发的所有者链接。脚本启动 Playwright 管理的 Chromium，用各自独立的桌面和触屏窄屏浏览器会话验证离线重连、授权、拒绝授权、证据验收和设备撤权。

```sh
npm --prefix web ci
npm --prefix web exec -- playwright install chromium
node scripts/p1/verify_web_ui.mjs --claim-url "$CLAIM_URL"
```

使用 `codex_harness.py` 的环境可追加 `--without-managed-test`，改测手机答复及文件差异验收，无需容器运行时。

---

## 4. 节点启动次数与证据核对方法

每个用例完成后，通过以下命令独立核对节点真实测试启动次数与归档状态：

```sh
sqlite3 /tmp/nextbot-p1-run/runner/runner.db "
SELECT j.id AS command_id,
       json_extract(j.body, '$.type') AS cmd_type,
       json_extract(j.body, '$.taskId') AS task_id,
       json_extract(j.body, '$.attemptId') AS attempt_id,
       j.phase,
       p.pid,
       p.observed,
       p.stopped
FROM jobs j
LEFT JOIN processes p ON p.command = j.id
WHERE json_extract(j.body, '$.type') = 'run_registered_test'
ORDER BY j.rowid;
"
```

- **正常批准 (`allow_once`)**：该 `attempt_id` 对应的 `processes` 行数必须为 **1**，且 `test-<requestId>.txt` 中包含 `[export-check] observed_run=true`、`workspace_readonly=true`、`all_checks=pass`。
- **审批拒绝 (`deny`) 与审批到期 (`expired`)**：该 `attempt_id` 对应的 `run_registered_test` 在 `processes` 中的记录行数必须为 **0**，且试图直接验收无测试证据的产物必须返回 `422 evidence_missing`。
- **与 GPT 分支合流重跑**：合入 `dev/gpt-runner-recovery` 后，继续使用同一个 `修复空数据导出失败并补回归测试` 任务，在 `run_registered_test` 完成到归档提交之间的窗口重启 Runner，确认重启后仍可找回 `test-<requestId>.json` / `test-<requestId>.txt` 且实际容器启动次数仍为 **1**。
