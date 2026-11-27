# NextBot 私网部署与维护

## Mac 与手机共用一个实例

Mac 版启动本机 Control 和 Runner。未配置手机接手时，Control 使用动态回环端口。启用手机接手后，Control 固定监听 `127.0.0.1:18787`。Mac 窗口仍使用本机入口，HTTPS 代理使用独立的私网来源，两个入口读取同一 SQLite 状态和事件流。初次所有者认领及恢复入口留在 Mac，手机使用短时一次性配对链接，浏览器会话使用 Secure、HttpOnly、SameSite=Strict Cookie。

先确保 Mac 和手机都可以解析私网地址。在 Mac 使用 [Caddy 模板](../../deploy/Caddyfile.example) 时，将 `NEXTBOT_HOST` 设为浏览器站点名、`NEXTBOT_PRIVATE_IP` 设为 Mac 的 LAN 或 VPN 地址、`NEXTBOT_UPSTREAM_PORT` 设为 `18787`。如使用 Caddy 内部 CA，把根证书导入客户端信任库。已有私网反向代理时，保留原始 Host，允许 SSE 长连接和及时刷新事件。

在桌面工具栏的**手机接手**中保存该 HTTPS 来源，进入**设置 → 可信设备**签发链接。手机配对后，在**待处理**中响应审批与问答，也可以检查差异和签收产物。Mac 需保持开机和 NextBot 运行，窗口可以关闭。关闭手机入口会停止该来源的访问。要撤销某台设备的会话，从可信设备列表撤销。

## Linux 长期运行

本例在专用非 root 账号下运行两个用户服务。主机需要 Git、所选 Agent CLI 及其专用登录目录。通过 [P1 Linux 验收](05-p1-linux-acceptance.md) 确认要启用的 CLI 和容器能力。在 Linux 主机从仓库根目录执行：

```sh
make build
install -d -m 700 "$HOME/.local/bin" "$HOME/.config/systemd/user" "$HOME/.config/nextbot"
install -m 755 bin/nextbot "$HOME/.local/bin/nextbot"
install -m 644 deploy/linux/*.service "$HOME/.config/systemd/user/"
install -m 600 deploy/linux/nextbot.env.example "$HOME/.config/nextbot/nextbot.env"
```

修改 `nextbot.env` 中的 HTTPS 来源、登录目录及经验证的 CLI 完整版本。值包含空格时可用双引号，目录使用绝对路径。默认要求新任务由独立 Agent 复核。启用受管测试前，确认此账号可运行 rootless Podman 或 Docker，再将 `NEXTBOT_MANAGED_TESTS` 设为 `true`。专用账号的 Git 工作区、模型凭据和 Runner 状态均由该账号管理。

```sh
systemctl --user daemon-reload
systemctl --user enable --now nextbot-control.service
systemctl --user status nextbot-control.service nextbot-runner.service
```

Control 启动时拉起 Runner。若主机要求用户登录后才能启动用户服务，用管理员权限对专用账号启用 linger：

```sh
sudo loginctl enable-linger "$USER"
```

### HTTPS、认领与状态

让反向代理仅监听 LAN 或 VPN 接口。使用 [Caddy 模板](../../deploy/Caddyfile.example) 时，`NEXTBOT_UPSTREAM_PORT` 使用 `8787`（或 `NEXTBOT_LISTEN` 中配置的端口）。`NEXTBOT_HOST` 必须与 `NEXTBOT_ORIGIN` 的主机和非默认端口一致。部署 Caddy 时向它的服务显式传入模板变量。已有证书和反向代理的主机也可以使用现有入口。Control 的 TCP 端口固定在回环，Runner 使用权限为 `0600` 的本机 Unix 套接字。

以运行服务的账号认领并登记仓库：

```sh
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/nextbot/control"
"$HOME/.local/bin/nextbot" claim --data "$STATE" --origin https://nextbot.example.internal
"$HOME/.local/bin/nextbot" project-add --data "$STATE" --repo /path/to/repository
```

在桌面打开认领命令返回的链接。检查 HTTPS 的 `/healthz` 返回成功，设置页显示节点在线，并用一个测试仓库演练派单、独立复核和手机签收。

### 维护与恢复

查看日志用 `journalctl --user -u nextbot-control -u nextbot-runner`。备份使用 `nextbot backup --data "$STATE" --out /path/to/new-backup`，再用 `verify-backup` 核对。Control 备份包含 SQLite 和受引用的归档。仓库、Runner 工作树和模型登录状态由主机单独备份。

恢复时先停止原服务并保存现有数据，再以 `restore --from /path/to/backup --data /path/to/new-control --source-instance <已核对实例ID>` 写到新的目录。验明归档后，将恢复目录切换到服务使用的状态路径，再启动服务并执行 `recover-owner` 重新绑定设备。恢复后先检查队列与未确认的尝试，再执行 `resume`。模型登录与仓库路径应在新主机上分别配置。

代理配置见 [Caddy 绑定网卡](https://caddyserver.com/docs/caddyfile/directives/bind) 与 [反向代理](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)。用户服务路径见 [systemd 单元文档](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html)。
