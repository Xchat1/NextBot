# NextBot

NextBot 面向自托管的多 Agent 编排与跨端控制。Mac 桌面应用负责本机仓库的派单、执行和验收，配对后的手机浏览器可以接手同一任务，也可将 Control 与 Runner 部署在自己的 Linux 主机上。

## Mac 桌面应用

在装有 Xcode 命令行工具、Go 1.25+ 和 Node.js 22.12+ 的 Mac 上运行 `make mac-app`，然后打开 `bin/NextBot.app`。应用会启动本机 Control 与 Runner，控制台只监听 Mac 的回环地址。

1. 首次打开时为这台 Mac 命名并绑定设备。
2. 从工具栏选择**添加仓库**，登记一个本机 Git 仓库。任务会使用独立的 Git worktree。
3. 使用 Codex 执行时，CLI 需已安装并登录。在**Agent 设置**填写登录目录与已验证的完整版本。保存后等节点就绪，再新建任务。使用 Claude Code 独立复核时，在同一界面填写其专用登录目录与已验证版本。

关闭窗口后任务继续运行。退出 NextBot 会停止本机服务，保存 Agent 或手机接手设置会重启服务。任务和日志保存在 `~/Library/Application Support/NextBot`。Mac 支持本机文件执行，受管容器测试使用 Linux 部署路径。

## 手机接手

1. 在 Mac 上用同一私网或 VPN 内的 HTTPS 反向代理转发到 `127.0.0.1:18787`，保留浏览器的 `Host`。可以使用 [Caddy 配置模板](deploy/Caddyfile.example)，让手机信任该站点的证书。
2. 在 Mac 工具栏选择**手机接手**，保存手机可访问的 HTTPS 来源，例如 `https://nextbot.example.internal`。在**设置 → 可信设备**生成一次性链接，用手机浏览器打开并绑定设备。
3. 关闭 Mac 窗口后，手机仍可查看任务、答复问题、处理授权及验收当前产物。要停止手机入口，在 Mac 设置中清空 HTTPS 来源。要使某台手机的会话失效，在可信设备中撤销该设备。

## Linux 构建与启动

构建需要 Linux、Go 1.25+、C 编译器和 Node.js 22.12+。发行二进制内嵌 Web 资源，执行主机另需 Git 和所选执行器。SQLite 使用随驱动构建的 SQLite 引擎。当前采用 `mattn/go-sqlite3`，工程稿所列的 `modernc.org/sqlite` 下载源在本次开发环境中不可访问。

```sh
make build
make test
```

长期运行请使用仓库提供的 [Linux 用户服务与私网部署说明](docs/engineering/06-private-deployment.md)，包含 systemd 单元、配置模板和恢复步骤。

在两个独立终端启动 Control 与 Runner。以下命令使用默认数据目录和 Unix 套接字。

```sh
bin/nextbot control --origin https://nextbot.example.internal
bin/nextbot runner --codex-home /path/to/dedicated/codex-profile
```

Control 监听 `127.0.0.1:8787`。用私网 DNS 或 VPN 配置浏览器地址，使桌面和手机都能验证 TLS 证书。以下是 Caddy 配置示例，使用 Caddy 内部 CA 时将其根证书加入两台设备的信任库。

```caddyfile
nextbot.example.internal {
	tls internal
	reverse_proxy 127.0.0.1:8787 {
		flush_interval -1
	}
}
```

仅在本机开发时，可以使用 `bin/nextbot control --dev-http --origin http://127.0.0.1:8787`。运行 `bin/nextbot claim --origin <实际浏览器来源>`，在桌面浏览器打开控制台输出的单次链接。通过设置页签发链接配对手机。部署可将两条启动命令配置为独立的系统服务。不同服务账号部署时，将套接字放在两账号可访问的专用目录，传入 `--runner-uid` 与 `--socket-gid`。

## 登记工作区与执行配置

在安装主机登记允许访问的 Git 仓库。任务从已确认的提交建立独立工作树。浏览器可先填写草稿，节点就绪后解析并确认基准。

```sh
bin/nextbot project-add --name "报告服务" --repo /path/to/repository
```

登记受管测试时传入 `--tests /path/to/tests.json`。以下清单中的镜像摘要需替换为管理员核验过、已下载的镜像摘要。镜像内程序从只读的 `/workspace` 读取冻结输入。临时文件与输出可写入 `/tmp` 与 `/output`。

```json
[
  {
    "testId": "export-check",
    "label": "导出回归验证",
    "image": "registry.example.internal/test-image@sha256:0000000000000000000000000000000000000000000000000000000000000000",
    "args": ["/bin/sh", "/workspace/test.sh"],
    "timeoutSeconds": 60
  }
]
```

准备一个可写的专用 Codex 登录目录。在合成仓库上按 G1–G3 验证文件编辑、终端限制、授权和取消后，启动 Runner 时传入 `--verified-codex-version`，值为 `codex --version` 的完整输出。节点还会检查登录和 stdio 握手。受管测试需要配置已验证的 rootless Podman 或 Docker，并传入 `--enable-managed-tests`。使用 Docker 时同时传入 `--container-engine docker`。

启用 P2 路径需在执行主机安装并登录 Claude Code，验证无工具的一次性只读配置，然后传入 `--claude-home` 与 `--verified-claude-version`。Mac 可在 **Agent 设置**要求独立复核，Linux Control 可传入 `--require-review`。此策略适用于新提交的任务。复核读取指定产物版本的归档内容，验收要求当前版本的通过结论。反向组合使用同一套验证用例，通过后可用 `--verified-claude-execution-version` 和 `--verified-codex-review-version` 再开放。

## 维护与恢复

- `bin/nextbot backup --out /path/to/new-backup` 生成 SQLite 一致性副本并保存全部已引用归档对象。使用 `bin/nextbot verify-backup --from /path/to/new-backup` 校验来源和内容。
- 恢复前隔离原 Control。使用 `bin/nextbot restore --from /path/to/new-backup --data /path/to/new-control --source-instance <验证过的实例 ID>` 恢复到新的数据目录，再用 `recover-owner --data <新目录> --origin <实际来源>` 绑定设备。
- `bin/nextbot pause` 和 `bin/nextbot resume` 控制新派发。外部动作结果未知时先检查 Runner 日志、原进程组及容器。确认停止后可使用 `bin/nextbot reconcile --task <任务 ID> --attempt <尝试 ID> --observation <核对依据>` 记录检查结果，并从任务页开启新尝试。
- Control 默认数据在 `.nextbot/control`，Runner 日志、工作树和 Git 派生分支保留在执行主机。设置页导出的诊断包括状态与关联标识，设备凭据和执行输出由原设备与受控归档接口管理。

## 工程开发文档

| 文档 | 读完可以决定什么 |
| --- | --- |
| [架构与技术选型](docs/engineering/01-architecture.md) | 如何在一台主机上部署控制服务与 Runner，以及哪些能力需要先做实测 |
| [接口与数据交换协议](docs/engineering/02-api-protocol.md) | 桌面、手机、控制服务与执行节点交换哪些请求、事件和回执 |
| [持久化与故障恢复](docs/engineering/03-data-recovery.md) | 任务、产物、审批存在哪里，断线、重启和重放时如何核对结果 |
| [P1 开发任务与验收计划](docs/engineering/04-p1-delivery.md) | 各项工作依赖什么，哪些证据齐全后可以进入内部试用 |
| [P1 Linux 验收](docs/engineering/05-p1-linux-acceptance.md) | 如何复验容器测试、双端交接和故障恢复 |
| [私网部署与维护](docs/engineering/06-private-deployment.md) | 如何启用 Mac 手机接手并在 Linux 持续运行 |

## 产品依据

- [产品需求文档](docs/product/01-prd.md) 定义用户、产品范围、竞品取舍和发布条件。
- [产品体验与操作说明](docs/product/02-product-guide.md) 描述一次任务从桌面派发到手机验收的过程。
- [功能规格与验收](docs/product/03-functional-spec.md) 规定任务状态、权限边界及各阶段验收用例。

P1 验证单用户、单节点、Codex 执行与人工验收。P2 在同一节点增加独立只读复核，作为首个可用发布版本。P3 再验证多节点委派与非代码产物。
