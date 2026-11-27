#!/usr/bin/env node
/**
 * Real browser (Desktop 1280x800 + Mobile 390x844) end-to-end UI acceptance test via Chrome DevTools Protocol.
 * Uses only Node 22+ built-ins (WebSocket, child_process, fs, os, path).
 */

import { spawn } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

function parseArgs(argv) {
  const out = {
    origin: '',
    claimUrl: '',
    chrome: process.env.CHROME_BIN || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    reportOut: '',
  }
  for (let i = 2; i < argv.length; i++) {
    const k = argv[i]
    const v = argv[i + 1]
    if (k === '--origin') { out.origin = v; i++ }
    else if (k === '--claim-url') { out.claimUrl = v; i++ }
    else if (k === '--chrome') { out.chrome = v; i++ }
    else if (k === '--report-out') { out.reportOut = v; i++ }
    else throw new Error(`Unknown arg: ${k}`)
  }
  if (!out.origin || !out.claimUrl) {
    throw new Error('Usage: verify_web_ui.mjs --origin URL --claim-url URL [--chrome PATH] [--report-out FILE]')
  }
  return out
}

async function waitFor(desc, fn, timeoutMs = 45000, intervalMs = 250) {
  const deadline = Date.now() + timeoutMs
  let lastErr
  while (Date.now() < deadline) {
    try {
      const val = await fn()
      if (val) return val
    } catch (err) {
      lastErr = err
    }
    await new Promise(r => setTimeout(r, intervalMs))
  }
  throw new Error(`Timed out waiting for ${desc} after ${timeoutMs}ms${lastErr ? `: ${lastErr.message}` : ''}`)
}

class CDPClient {
  constructor(wsUrl) {
    this.ws = new WebSocket(wsUrl)
    this.nextId = 1
    this.pending = new Map()
    this.ready = new Promise((resolve, reject) => {
      this.ws.addEventListener('open', resolve, { once: true })
      this.ws.addEventListener('error', reject, { once: true })
    })
    this.ws.addEventListener('message', ev => {
      const msg = JSON.parse(String(ev.data))
      if (msg.id && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id)
        this.pending.delete(msg.id)
        if (msg.error) reject(new Error(`${msg.error.code}: ${msg.error.message}`))
        else resolve(msg.result)
      }
    })
  }

  async call(method, params = {}) {
    await this.ready
    const id = this.nextId++
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      this.ws.send(JSON.stringify({ id, method, params }))
    })
  }

  async eval(expression) {
    const res = await this.call('Runtime.evaluate', {
      expression,
      awaitPromise: true,
      returnByValue: true,
    })
    if (res.exceptionDetails) {
      throw new Error(`Page JS error: ${res.exceptionDetails.text || JSON.stringify(res.exceptionDetails)}`)
    }
    return res.result?.value
  }

  async close() {
    this.ws.close()
  }
}

async function launchChrome(chromePath, userDataDir, url) {
  const proc = spawn(chromePath, [
    '--headless=new',
    '--no-sandbox',
    '--disable-gpu',
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-session-crashed-bubble',
    '--remote-debugging-port=0',
    `--user-data-dir=${userDataDir}`,
    url,
  ], { stdio: ['ignore', 'pipe', 'pipe'] })

  let stderr = ''
  const wsEndpoint = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`Chrome did not print DevTools URL: ${stderr}`)), 15000)
    const onData = chunk => {
      stderr += chunk.toString()
      const m = stderr.match(/DevTools listening on (ws:\/\/[^\s]+)/)
      if (m) {
        clearTimeout(timer)
        resolve(m[1])
      }
    }
    proc.stderr.on('data', onData)
    proc.on('exit', code => {
      clearTimeout(timer)
      reject(new Error(`Chrome exited (${code}): ${stderr}`))
    })
  })

  const port = new URL(wsEndpoint).port
  const targets = await waitFor('page target', async () => {
    const res = await fetch(`http://127.0.0.1:${port}/json/list`)
    const list = await res.json()
    return list.find(t => t.type === 'page' && t.url && t.url.startsWith('http')) || list.find(t => t.type === 'page')
  }, 10000, 150)

  const cdp = new CDPClient(targets.webSocketDebuggerUrl)
  await cdp.call('Page.enable')
  await cdp.call('Runtime.enable')
  return { proc, cdp, port }
}

async function stopChrome(instance) {
  if (!instance) return
  try { await instance.cdp.call('Browser.close') } catch {}
  try { await instance.cdp.close() } catch {}
  if (!instance.proc.killed && instance.proc.exitCode === null) {
    await new Promise(resolve => {
      const timer = setTimeout(() => {
        try { instance.proc.kill('SIGTERM') } catch {}
        setTimeout(() => { try { instance.proc.kill('SIGKILL') } catch {}; resolve() }, 2000)
      }, 2500)
      instance.proc.once('exit', () => { clearTimeout(timer); resolve() })
    })
  }
}

async function setReactValue(cdp, selector, value) {
  const ok = await cdp.eval(`(() => {
    const el = document.querySelector(${JSON.stringify(selector)});
    if (!el) return false;
    const proto = el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : el.tagName === 'SELECT' ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
    const setter = Object.getOwnPropertyDescriptor(proto, 'value')?.set;
    if (setter) setter.call(el, ${JSON.stringify(value)});
    else el.value = ${JSON.stringify(value)};
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
    return true;
  })()`)
  if (!ok) throw new Error(`Element not found: ${selector}`)
}

async function clickByText(cdp, selector, textMatch) {
  const clicked = await cdp.eval(`(() => {
    const nodes = Array.from(document.querySelectorAll(${JSON.stringify(selector)}));
    const el = nodes.find(n => (n.textContent || '').includes(${JSON.stringify(textMatch)}) && !n.disabled);
    if (!el) return false;
    el.click();
    return true;
  })()`)
  if (!clicked) throw new Error(`Clickable element not found: ${selector} containing "${textMatch}"`)
}

async function checkViewportFit(cdp, label) {
  const metrics = await cdp.eval(`(() => ({
    innerWidth: window.innerWidth,
    scrollWidth: document.documentElement.scrollWidth,
    bodyTextLen: document.body.innerText.length
  }))()`)
  if (metrics.scrollWidth > metrics.innerWidth + 1) {
    throw new Error(`Horizontal overflow at ${label}: scrollWidth=${metrics.scrollWidth} > innerWidth=${metrics.innerWidth}`)
  }
  return metrics
}

async function main() {
  const args = parseArgs(process.argv)
  const tmpRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'nextbot-ui-'))
  const desktopDir = path.join(tmpRoot, 'desktop-profile')
  const mobileDir = path.join(tmpRoot, 'mobile-profile')
  fs.mkdirSync(desktopDir)
  fs.mkdirSync(mobileDir)

  const layoutChecks = []
  let desktop, mobile
  try {
    // 1. Desktop claims owner
    desktop = await launchChrome(args.chrome, desktopDir, args.claimUrl)
    await desktop.cdp.call('Emulation.setDeviceMetricsOverride', { width: 1280, height: 800, deviceScaleFactor: 1, mobile: false })
    await waitFor('claim form', () => desktop.cdp.eval(`!!document.querySelector('input[placeholder="例如，办公室电脑"]')`))
    await setReactValue(desktop.cdp, 'input[placeholder="例如，办公室电脑"]', 'Desktop Chrome')
    await clickByText(desktop.cdp, 'button.primary', '绑定此设备')
    await waitFor('desktop realtime sync and node online', () => desktop.cdp.eval(`
      document.body.innerText.includes('实时同步') && document.body.innerText.includes('节点在线')
    `))
    layoutChecks.push({ view: 'desktop-dashboard', ...(await checkViewportFit(desktop.cdp, 'desktop-dashboard')) })

    // 2. Desktop generates pairing URL
    await desktop.cdp.eval(`location.hash = 'settings'`)
    await waitFor('settings pair button', () => desktop.cdp.eval(`Array.from(document.querySelectorAll('button')).some(b => b.textContent.includes('配对设备'))`))
    await clickByText(desktop.cdp, 'button', '配对设备')
    const pairUrl = await waitFor('pairing URL in modal', () => desktop.cdp.eval(`document.querySelector('input[readonly]')?.value || ''`))
    if (!pairUrl.includes('#pair=')) throw new Error(`Unexpected pairUrl: ${pairUrl}`)

    // 3. Mobile (390x844, plus 375x667 narrow check) claims pairing
    mobile = await launchChrome(args.chrome, mobileDir, pairUrl)
    await mobile.cdp.call('Network.enable')
    await mobile.cdp.call('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 2, mobile: true })
    await waitFor('mobile bind form', () => mobile.cdp.eval(`!!document.querySelector('input[placeholder="例如，办公室电脑"]')`))
    await setReactValue(mobile.cdp, 'input[placeholder="例如，办公室电脑"]', 'iPhone Viewport')
    await clickByText(mobile.cdp, 'button.primary', '绑定此设备')
    await waitFor('mobile realtime sync', () => mobile.cdp.eval(`document.body.innerText.includes('实时同步')`))
    layoutChecks.push({ view: 'mobile-dashboard-390', ...(await checkViewportFit(mobile.cdp, 'mobile-dashboard-390')) })
    await mobile.cdp.call('Emulation.setDeviceMetricsOverride', { width: 375, height: 667, deviceScaleFactor: 2, mobile: true })
    layoutChecks.push({ view: 'mobile-dashboard-375', ...(await checkViewportFit(mobile.cdp, 'mobile-dashboard-375')) })
    await mobile.cdp.call('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 2, mobile: true })

    // 4. Desktop creates and submits demo task, then closes page
    await desktop.cdp.eval(`location.hash = 'new'`)
    await waitFor('new task form', () => desktop.cdp.eval(`!!document.querySelector('select[aria-label="项目仓库"]')?.value`))
    await setReactValue(desktop.cdp, 'input[placeholder="例如，修复报告导出失败"]', '修复空数据导出失败并补回归测试')
    await setReactValue(desktop.cdp, 'textarea[placeholder="需要交付什么？"]', '修复 src/export_report.sh 在空数据输入时导出失败的问题，保留原有 CSV 列序并补回归测试。')
    await setReactValue(desktop.cdp, 'textarea[placeholder="哪些目录、文件或行为可以改动？"]', '仅修改 src/export_report.sh 与 tests/ 目录。')
    await setReactValue(desktop.cdp, 'textarea[placeholder="用哪些证据判断任务完成？"]', '普通和空数据输入均返回退出码 0 且输出一致，受管测试 export-check 通过。')
    await clickByText(desktop.cdp, 'button', '解析 Git 基准')
    await waitFor('resolved commit', () => desktop.cdp.eval(`!!document.querySelector('.commit-confirm code')?.textContent?.trim()`))
    await setReactValue(desktop.cdp, 'select[aria-label="受管测试"]', 'export-check')
    await desktop.cdp.eval(`(() => {
      const cb = document.querySelector('input[type="checkbox"]');
      if (cb && !cb.checked) cb.click();
    })()`)
    await waitFor('submit enabled', () => desktop.cdp.eval(`!Array.from(document.querySelectorAll('button[type="submit"]')).find(b => b.textContent.includes('提交并排队'))?.disabled`))
    await clickByText(desktop.cdp, 'button[type="submit"]', '提交并排队')
    const taskHash = await waitFor('navigated to #task/', () => desktop.cdp.eval(`location.hash.startsWith('#task/') ? location.hash : ''`))
    const taskId = taskHash.replace('#task/', '')

    // Close Desktop browser!
    await stopChrome(desktop)
    desktop = null

    // 5. Mobile approves in narrow screen, views evidence, accepts v1
    await mobile.cdp.eval(`location.hash = 'inbox'`)
    await waitFor('mobile inbox approval', () => mobile.cdp.eval(`document.body.innerText.includes('测试授权')`))
    layoutChecks.push({ view: 'mobile-inbox', ...(await checkViewportFit(mobile.cdp, 'mobile-inbox')) })
    await clickByText(mobile.cdp, 'a.task-row', '测试授权')
    await waitFor('mobile pending approval card', () => mobile.cdp.eval(`
      document.body.innerText.includes('空数据导出回归验证') && document.body.innerText.includes('export_check.sh')
    `))
    layoutChecks.push({ view: 'mobile-approval', ...(await checkViewportFit(mobile.cdp, 'mobile-approval')) })
    await clickByText(mobile.cdp, 'button.primary', '允许本次')
    await waitFor('confirm modal', () => mobile.cdp.eval(`Array.from(document.querySelectorAll('button.primary')).some(b => b.textContent.includes('确认授权'))`))
    await clickByText(mobile.cdp, 'button.primary', '确认授权')

    await waitFor('mobile awaiting_acceptance', () => mobile.cdp.eval(`document.body.innerText.includes('v1 待验收')`), 90000)
    await clickByText(mobile.cdp, 'button', '查看当前版本证据')
    await waitFor('artifact entries', () => mobile.cdp.eval(`document.body.innerText.includes('实际测试记录')`))
    layoutChecks.push({ view: 'mobile-artifact-tab', ...(await checkViewportFit(mobile.cdp, 'mobile-artifact-tab')) })

    await clickByText(mobile.cdp, 'button.entry-button', '实际测试记录')
    await waitFor('observed_run JSON preview', () => mobile.cdp.eval(`(document.querySelector('.evidence pre')?.textContent || '').includes('"exitCode": 0')`))
    await clickByText(mobile.cdp, 'button.entry-button', '测试输出')
    await waitFor('execution log preview', () => mobile.cdp.eval(`(document.querySelector('.evidence pre')?.textContent || '').includes('[export-check] all_checks=pass')`))

    await clickByText(mobile.cdp, 'button.primary', '接受 v1')
    await waitFor('confirm accept modal', () => mobile.cdp.eval(`Array.from(document.querySelectorAll('button.primary')).some(b => b.textContent.includes('确认接受'))`))
    await clickByText(mobile.cdp, 'button.primary', '确认接受')
    await waitFor('mobile task completed', () => mobile.cdp.eval(`document.body.innerText.includes('已完成')`))
    layoutChecks.push({ view: 'mobile-completed', ...(await checkViewportFit(mobile.cdp, 'mobile-completed')) })

    // 6. Reopen Desktop and confirm completed state + acceptance record visible
    desktop = await launchChrome(args.chrome, desktopDir, `${args.origin}/#task/${taskId}`)
    await desktop.cdp.call('Emulation.setDeviceMetricsOverride', { width: 1280, height: 800, deviceScaleFactor: 1, mobile: false })
    await waitFor('desktop sees completed task', () => desktop.cdp.eval(`
      document.body.innerText.includes('已完成') && document.body.innerText.includes('最近验收') && document.body.innerText.includes('已接受')
    `))
    layoutChecks.push({ view: 'desktop-completed', ...(await checkViewportFit(desktop.cdp, 'desktop-completed')) })

    // 7. Mobile offline + Desktop submits Deny-test task -> Mobile reconnects -> Mobile denies -> accept disabled
    await mobile.cdp.call('Network.emulateNetworkConditions', { offline: true, latency: 0, downloadThroughput: -1, uploadThroughput: -1 })
    await waitFor('mobile detects offline', () => mobile.cdp.eval(`document.body.innerText.includes('正在连接')`), 10000)

    await desktop.cdp.eval(`location.hash = 'new'`)
    await waitFor('new task form for deny', () => desktop.cdp.eval(`!!document.querySelector('select[aria-label="项目仓库"]')?.value`))
    await setReactValue(desktop.cdp, 'input[placeholder="例如，修复报告导出失败"]', '空数据导出拒绝授权验证')
    await setReactValue(desktop.cdp, 'textarea[placeholder="需要交付什么？"]', '验证手机离线重连后在窄屏拒绝授权并阻断无证据验收。')
    await setReactValue(desktop.cdp, 'textarea[placeholder="哪些目录、文件或行为可以改动？"]', 'src/export_report.sh')
    await setReactValue(desktop.cdp, 'textarea[placeholder="用哪些证据判断任务完成？"]', '拒绝后不得启动容器且不能验收。')
    await clickByText(desktop.cdp, 'button', '解析 Git 基准')
    await waitFor('resolved commit deny', () => desktop.cdp.eval(`!!document.querySelector('.commit-confirm code')?.textContent?.trim()`))
    await setReactValue(desktop.cdp, 'select[aria-label="受管测试"]', 'export-check')
    await desktop.cdp.eval(`(() => { const cb = document.querySelector('input[type="checkbox"]'); if (cb && !cb.checked) cb.click(); })()`)
    await waitFor('submit enabled deny', () => desktop.cdp.eval(`!Array.from(document.querySelectorAll('button[type="submit"]')).find(b => b.textContent.includes('提交并排队'))?.disabled`))
    await clickByText(desktop.cdp, 'button[type="submit"]', '提交并排队')
    const denyHash = await waitFor('navigated to deny #task/', () => desktop.cdp.eval(`location.hash.startsWith('#task/') && location.hash !== '#task/${taskId}' ? location.hash : ''`))
    const denyTaskId = denyHash.replace('#task/', '')

    // Mobile reconnects and sees the new pending approval in Inbox
    await mobile.cdp.call('Network.emulateNetworkConditions', { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 })
    await waitFor('mobile reconnected realtime sync', () => mobile.cdp.eval(`document.body.innerText.includes('实时同步')`), 15000)
    await mobile.cdp.eval(`location.hash = 'task/${denyTaskId}'`)
    await waitFor('mobile sees deny pending approval', () => mobile.cdp.eval(`document.body.innerText.includes('空数据导出回归验证') && Array.from(document.querySelectorAll('button')).some(b => b.textContent.trim() === '拒绝')`), 30000)
    await clickByText(mobile.cdp, 'button.secondary', '拒绝')
    await waitFor('mobile sees missing evidence notice and disabled accept', () => mobile.cdp.eval(`
      document.body.innerText.includes('当前版本缺少验收所需证据') &&
      Array.from(document.querySelectorAll('button.primary')).some(b => b.textContent.includes('接受') && b.disabled)
    `), 30000)

    // 8. Desktop revokes Mobile in Settings -> Mobile automatically signs out to Bind screen
    await desktop.cdp.eval(`location.hash = 'settings'`)
    await waitFor('settings device list', () => desktop.cdp.eval(`document.body.innerText.includes('iPhone Viewport')`))
    await desktop.cdp.eval(`(() => {
      const items = Array.from(document.querySelectorAll('.settings-item'));
      const row = items.find(el => el.textContent.includes('iPhone Viewport'));
      const btn = row && Array.from(row.querySelectorAll('button')).find(b => b.textContent.trim() === '撤销');
      if (!btn) throw new Error('Revoke button for iPhone Viewport not found');
      btn.click();
    })()`)
    await waitFor('confirm revoke modal', () => desktop.cdp.eval(`Array.from(document.querySelectorAll('button.destructive')).some(b => b.textContent.includes('撤销访问'))`))
    await clickByText(desktop.cdp, 'button.destructive', '撤销访问')
    await waitFor('mobile returned to bind screen after revoke', () => mobile.cdp.eval(`document.body.innerText.includes('连接你的设备')`), 15000)

    const out = { status: 'PASS', taskId, denyTaskId, origin: args.origin, layoutChecks }
    if (args.reportOut) {
      fs.mkdirSync(path.dirname(args.reportOut), { recursive: true })
      fs.writeFileSync(args.reportOut, JSON.stringify(out, null, 2) + '\n')
    }
    console.log(JSON.stringify(out, null, 2))
  } catch (err) {
    if (desktop) {
      const dInfo = await desktop.cdp.eval(`({ href: location.href, text: document.body.innerText.slice(0, 600) })`).catch(e => String(e))
      console.error('DESKTOP STATE ON ERROR:', dInfo)
    }
    if (mobile) {
      const mInfo = await mobile.cdp.eval(`({ href: location.href, text: document.body.innerText.slice(0, 600) })`).catch(e => String(e))
      console.error('MOBILE STATE ON ERROR:', mInfo)
    }
    throw err
  } finally {
    await stopChrome(mobile)
    await stopChrome(desktop)
    fs.rmSync(tmpRoot, { recursive: true, force: true })
  }
}

main().catch(err => {
  console.error(err)
  process.exit(1)
})
