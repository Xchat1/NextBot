import AppKit
import WebKit

private struct LocalEndpoint: Decodable {
    let origin: URL
    let ticket: String
}

@MainActor
private final class NextBotApp: NSObject, NSApplicationDelegate, WKNavigationDelegate, NSToolbarDelegate {
    private let defaults = UserDefaults.standard
    private let fileManager = FileManager.default
    private let window = NSWindow(
        contentRect: NSRect(x: 0, y: 0, width: 1120, height: 760),
        styleMask: [.titled, .closable, .miniaturizable, .resizable],
        backing: .buffered, defer: false
    )
    private let webView = WKWebView(frame: .zero, configuration: WKWebViewConfiguration())
    private var backend: Process?
    private var endpoint: LocalEndpoint?
    private var startupOutput = Data()
    private var restarting = false
    private var quitting = false

    private var dataDirectory: URL {
        if let path = ProcessInfo.processInfo.environment["NEXTBOT_MACOS_STATE_DIR"], !path.isEmpty {
            return URL(fileURLWithPath: path, isDirectory: true)
        }
        return fileManager.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("NextBot", isDirectory: true)
    }

    private var controlDirectory: String {
        dataDirectory.appendingPathComponent("control", isDirectory: true).path
    }

    private var logURL: URL { dataDirectory.appendingPathComponent("NextBot.log") }

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.mainMenu = makeMenu()
        window.title = "NextBot"
        window.minSize = NSSize(width: 760, height: 540)
        window.isReleasedWhenClosed = false
        let toolbar = NSToolbar(identifier: "NextBotWorkspace")
        toolbar.delegate = self
        toolbar.displayMode = .iconAndLabel
        window.toolbar = toolbar
        window.center()
        webView.navigationDelegate = self
        webView.configuration.userContentController.addUserScript(
            WKUserScript(source: "window.nextBotDesktop = true", injectionTime: .atDocumentStart, forMainFrameOnly: true)
        )
        if #available(macOS 13.3, *) {
            webView.isInspectable = ProcessInfo.processInfo.environment["NEXTBOT_MACOS_INSPECT"] == "1"
        }
        startBackend()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows: Bool) -> Bool {
        window.makeKeyAndOrderFront(nil)
        return true
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        guard let backend, backend.isRunning else { return .terminateNow }
        quitting = true
        backend.terminate()
        return .terminateLater
    }

    private func makeMenu() -> NSMenu {
        let bar = NSMenu()
        let app = NSMenu(title: "NextBot")
        app.addItem(withTitle: "关于 NextBot", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        app.addItem(.separator())
        app.addItem(withTitle: "退出 NextBot", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        addMenu(app, to: bar)
        let edit = NSMenu(title: "编辑")
        edit.addItem(withTitle: "撤销", action: Selector(("undo:")), keyEquivalent: "z")
        edit.addItem(withTitle: "剪切", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "复制", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "粘贴", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "全选", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        addMenu(edit, to: bar)
        let workspace = NSMenu(title: "工作区")
        for (title, action, key) in [
            ("添加本机仓库…", #selector(addRepository), "o"),
            ("Codex 设置…", #selector(configureCodex), ","),
            ("重新载入控制台", #selector(reloadConsole), "r"),
            ("重新绑定本机…", #selector(recoverOwner), ""),
            ("查看本机日志", #selector(openLog), "")
        ] {
            workspace.addItem(withTitle: title, action: action, keyEquivalent: key).target = self
        }
        addMenu(workspace, to: bar)
        return bar
    }

    private func addMenu(_ menu: NSMenu, to bar: NSMenu) {
        let item = NSMenuItem()
        item.submenu = menu
        bar.addItem(item)
    }

    func toolbarDefaultItemIdentifiers(_ toolbar: NSToolbar) -> [NSToolbarItem.Identifier] {
        [.init("repository"), .flexibleSpace, .init("codex"), .init("reload")]
    }

    func toolbarAllowedItemIdentifiers(_ toolbar: NSToolbar) -> [NSToolbarItem.Identifier] {
        toolbarDefaultItemIdentifiers(toolbar)
    }

    func toolbar(_ toolbar: NSToolbar, itemForItemIdentifier identifier: NSToolbarItem.Identifier,
                 willBeInsertedIntoToolbar flag: Bool) -> NSToolbarItem? {
        let label: String
        let icon: String
        let action: Selector
        switch identifier.rawValue {
        case "repository": (label, icon, action) = ("添加仓库", "folder.badge.plus", #selector(addRepository))
        case "codex": (label, icon, action) = ("Codex 设置", "gearshape", #selector(configureCodex))
        case "reload": (label, icon, action) = ("重新载入", "arrow.clockwise", #selector(reloadConsole))
        default: return nil
        }
        let item = NSToolbarItem(itemIdentifier: identifier)
        item.label = label
        item.image = NSImage(systemSymbolName: icon, accessibilityDescription: label)
        item.target = self
        item.action = action
        return item
    }

    private func makeProcess(_ arguments: [String]) throws -> Process {
        guard let binary = Bundle.main.url(forResource: "nextbot", withExtension: nil) else {
            throw CocoaError(.fileNoSuchFile)
        }
        try fileManager.createDirectory(at: dataDirectory, withIntermediateDirectories: true,
                                        attributes: [.posixPermissions: 0o700])
        var environment = ProcessInfo.processInfo.environment
        let paths = ["/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"]
        let existing = (environment["PATH"] ?? "").split(separator: ":").map(String.init)
        environment["PATH"] = (existing + paths).joined(separator: ":")
        let process = Process()
        process.executableURL = binary
        process.arguments = arguments
        process.environment = environment
        process.currentDirectoryURL = dataDirectory
        return process
    }

    private func startBackend() {
        endpoint = nil
        startupOutput.removeAll()
        showStatus("正在启动 NextBot", detail: "正在连接这台 Mac 上的控制服务。")
        do {
            let process = try makeProcess([
                "desktop", "--data", dataDirectory.path,
                "--parent-pid", String(ProcessInfo.processInfo.processIdentifier),
                "--codex-home", defaults.string(forKey: "codexHome") ?? "",
                "--verified-codex-version", defaults.string(forKey: "codexVersion") ?? ""
            ])
            if !fileManager.fileExists(atPath: logURL.path) {
                fileManager.createFile(atPath: logURL.path, contents: nil, attributes: [.posixPermissions: 0o600])
            }
            let logHandle = try FileHandle(forWritingTo: logURL)
            try logHandle.seekToEnd()
            let ready = Pipe()
            process.standardOutput = ready
            process.standardError = logHandle
            process.terminationHandler = { [weak self] process in
                ready.fileHandleForReading.readabilityHandler = nil
                try? logHandle.close()
                DispatchQueue.main.async {
                    self?.backendExited(process)
                }
            }
            ready.fileHandleForReading.readabilityHandler = { [weak self] handle in
                let data = handle.availableData
                guard !data.isEmpty else {
                    handle.readabilityHandler = nil
                    return
                }
                DispatchQueue.main.async {
                    self?.receiveEndpoint(data, from: process)
                }
            }
            do {
                try process.run()
            } catch {
                ready.fileHandleForReading.readabilityHandler = nil
                try? logHandle.close()
                throw error
            }
            backend = process
        } catch {
            showStatus("启动失败", detail: error.localizedDescription, retry: true)
        }
    }

    private func receiveEndpoint(_ data: Data, from process: Process) {
        guard backend === process, endpoint == nil else { return }
        startupOutput.append(data)
        guard let end = startupOutput.firstIndex(of: 10) else { return }
        defer { startupOutput.removeAll() }
        do {
            let endpoint = try JSONDecoder().decode(LocalEndpoint.self, from: startupOutput.prefix(upTo: end))
            guard endpoint.origin.scheme == "http", endpoint.origin.host == "127.0.0.1", endpoint.origin.port != nil else {
                throw CocoaError(.fileReadCorruptFile)
            }
            self.endpoint = endpoint
            var url = URLComponents(url: endpoint.origin, resolvingAgainstBaseURL: false)
            if !endpoint.ticket.isEmpty { url?.fragment = "claim=" + endpoint.ticket }
            if let target = url?.url {
                window.contentView = webView
                webView.load(URLRequest(url: target))
            }
        } catch {
            showStatus("无法连接本机控制台", detail: "启动响应格式无效。", retry: true)
        }
    }

    private func backendExited(_ process: Process) {
        guard backend === process else { return }
        backend = nil
        endpoint = nil
        if quitting {
            NSApp.reply(toApplicationShouldTerminate: true)
        } else if restarting {
            restarting = false
            startBackend()
        } else {
            showStatus("本机服务已停止", detail: "退出状态 (process.terminationStatus)。如需排查，请查看本机日志。", retry: true)
        }
    }

    private func showStatus(_ title: String, detail: String, retry: Bool = false) {
        let heading = NSTextField(labelWithString: title)
        heading.font = .systemFont(ofSize: 23, weight: .semibold)
        let message = NSTextField(wrappingLabelWithString: detail)
        message.textColor = .secondaryLabelColor
        let stack = NSStackView(views: [heading, message])
        stack.orientation = .vertical
        stack.alignment = .centerX
        stack.spacing = 12
        if retry {
            stack.addArrangedSubview(NSButton(title: "重试", target: self, action: #selector(reloadConsole)))
        }
        let view = NSView()
        stack.translatesAutoresizingMaskIntoConstraints = false
        view.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.centerXAnchor.constraint(equalTo: view.centerXAnchor),
            stack.centerYAnchor.constraint(equalTo: view.centerYAnchor),
            stack.widthAnchor.constraint(lessThanOrEqualToConstant: 500)
        ])
        window.contentView = view
    }

    @objc private func reloadConsole() {
        if let endpoint {
            let current = webView.url ?? endpoint.origin
            window.contentView = webView
            webView.load(URLRequest(url: current))
        } else if backend?.isRunning == true {
            restarting = true
            backend?.terminate()
        } else {
            startBackend()
        }
    }

    @objc private func addRepository() {
        let panel = NSOpenPanel()
        panel.title = "添加本机 Git 仓库"
        panel.canChooseFiles = false
        panel.canChooseDirectories = true
        guard panel.runModal() == .OK, let url = panel.url else { return }
        runTool(["project-add", "--data", controlDirectory, "--repo", url.path]) { [weak self] _ in
            self?.reloadConsole()
        }
    }

    @objc private func recoverOwner() {
        guard let endpoint else { return }
        let alert = NSAlert()
        alert.messageText = "重新绑定这台 Mac？"
        alert.informativeText = "控制台会撤销现有设备会话，并签发新的认领凭据。任务和产物会保留。"
        alert.addButton(withTitle: "重新绑定")
        alert.addButton(withTitle: "取消")
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        runTool(["recover-owner", "--data", controlDirectory, "--origin", endpoint.origin.absoluteString]) { [weak self] output in
            guard let self, let url = URL(string: output.trimmingCharacters(in: .whitespacesAndNewlines)) else { return }
            self.webView.load(URLRequest(url: url))
        }
    }

    @objc private func configureCodex() {
        let home = NSTextField(string: defaults.string(forKey: "codexHome") ?? "")
        home.placeholderString = "~/.codex（留空使用 CLI 默认目录）"
        let version = NSTextField(string: defaults.string(forKey: "codexVersion") ?? "")
        version.placeholderString = "codex --version 的完整输出"
        let stack = NSStackView(views: [NSTextField(labelWithString: "Codex 登录目录"), home,
                                        NSTextField(labelWithString: "已验证的 Codex 版本"), version])
        stack.orientation = .vertical
        stack.alignment = .leading
        home.widthAnchor.constraint(equalToConstant: 340).isActive = true
        version.widthAnchor.constraint(equalTo: home.widthAnchor).isActive = true
        let alert = NSAlert()
        alert.messageText = "本机 Codex 设置"
        alert.informativeText = "应用会使用该目录中的 CLI 登录。填入已验证版本后可启用文件执行。保存会重启本机服务，可能中断正在运行的任务。"
        alert.accessoryView = stack
        alert.addButton(withTitle: "保存并重启")
        alert.addButton(withTitle: "取消")
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        defaults.set((home.stringValue.trimmingCharacters(in: .whitespacesAndNewlines) as NSString).expandingTildeInPath, forKey: "codexHome")
        defaults.set(version.stringValue.trimmingCharacters(in: .whitespacesAndNewlines), forKey: "codexVersion")
        if let backend, backend.isRunning {
            restarting = true
            backend.terminate()
        } else {
            startBackend()
        }
    }

    @objc private func openLog() { NSWorkspace.shared.open(logURL) }

    private func runTool(_ arguments: [String], onSuccess: @escaping (String) -> Void) {
        do {
            let process = try makeProcess(arguments)
            let output = Pipe()
            process.standardOutput = output
            process.standardError = output
            try process.run()
            DispatchQueue.global().async {
                let data = output.fileHandleForReading.readDataToEndOfFile()
                process.waitUntilExit()
                let result = String(decoding: data, as: UTF8.self)
                DispatchQueue.main.async {
                    if process.terminationStatus == 0 {
                        onSuccess(result)
                    } else {
                        let alert = NSAlert()
                        alert.messageText = "操作失败"
                        alert.informativeText = result
                        alert.runModal()
                    }
                }
            }
        } catch {
            NSAlert(error: error).runModal()
        }
    }

    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let origin = endpoint?.origin, let url = action.request.url, action.targetFrame != nil,
              url.scheme == origin.scheme, url.host == origin.host, url.port == origin.port else {
            decisionHandler(.cancel)
            return
        }
        decisionHandler(.allow)
    }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        if (error as NSError).code != NSURLErrorCancelled {
            showStatus("控制台连接失败", detail: error.localizedDescription, retry: true)
        }
    }
}

@main
private struct NextBotMain {
    @MainActor
    static func main() {
        let app = NSApplication.shared
        let delegate = NextBotApp()
        app.delegate = delegate
        app.setActivationPolicy(.regular)
        withExtendedLifetime(delegate) { app.run() }
    }
}
