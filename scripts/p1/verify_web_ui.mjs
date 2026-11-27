#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { chromium } from '../../web/node_modules/playwright/index.mjs'

const { values: options } = parseArgs({
  options: {
    'claim-url': { type: 'string' },
    'origin': { type: 'string' },
    'report-out': { type: 'string' },
    'without-managed-test': { type: 'boolean' },
  },
})
if (!options['claim-url']) {
  throw new Error('Usage: verify_web_ui.mjs --claim-url URL [--origin URL] [--without-managed-test] [--report-out FILE]')
}
const claimUrl = new URL(options['claim-url'])
const origin = options.origin ?? claimUrl.origin
if (claimUrl.origin !== origin || !claimUrl.hash.startsWith('#claim=')) {
  throw new Error('Pass the one-time owner URL for the acceptance instance')
}
const managed = !options['without-managed-test']
const layoutChecks = []
const text = (page, label) => page.getByText(label, { exact: true })
const button = (page, label) => page.getByRole('button', { name: label, exact: true })
const wait = (locator, timeout = 45000) => locator.waitFor({ state: 'visible', timeout })

async function route(page, hash) {
  await page.evaluate(hash => { location.hash = hash }, hash)
}
async function bind(page, url, name) {
  await page.goto(url)
  await page.getByPlaceholder('例如，办公室电脑').fill(name)
  await button(page, '绑定此设备').click()
  await wait(text(page, '实时同步'))
}
async function checkViewport(page, view) {
  const metrics = await page.evaluate(() => ({
    innerWidth, scrollWidth: document.documentElement.scrollWidth,
  }))
  if (metrics.scrollWidth > metrics.innerWidth + 1) {
    throw new Error(`Horizontal overflow at ${view}: ${metrics.scrollWidth} > ${metrics.innerWidth}`)
  }
  layoutChecks.push({ view, ...metrics })
}
async function submitFixture(page, title, goal) {
  await route(page, 'new')
  await page.getByPlaceholder('例如，修复报告导出失败').fill(title)
  await page.getByPlaceholder('需要交付什么？').fill(goal)
  await page.getByPlaceholder('哪些目录、文件或行为可以改动？').fill('仅修改 src/export_report.sh 与 tests/ 目录。')
  await page.getByPlaceholder('用哪些证据判断任务完成？').fill('普通和空数据输入均返回退出码 0 且输出一致。')
  await button(page, '解析 Git 基准').click()
  await wait(page.locator('.commit-confirm code'))
  if (managed) {
    await page.getByLabel('受管测试', { exact: true }).selectOption('export-check')
    await page.getByRole('checkbox', { name: '验收要求本版本有成功的实际测试记录' }).check()
  }
  await button(page, '提交并排队').click()
  await page.waitForURL(/#task\/[a-f0-9]+$/u)
  return new URL(page.url()).hash
}

const browser = await chromium.launch({ headless: true })
try {
  const desktop = await browser.newContext({ viewport: { width: 1280, height: 800 } })
  const mobile = await browser.newContext({
    viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2,
  })
  desktop.setDefaultTimeout(45000)
  mobile.setDefaultTimeout(45000)
  const work = await desktop.newPage()
  const phone = await mobile.newPage()
  await bind(work, claimUrl.href, '验收电脑')
  await wait(work.getByText('节点在线').first())
  await checkViewport(work, 'desktop-dashboard')

  await route(work, 'settings')
  await button(work, '配对设备').click()
  const pairUrl = new URL(await work.getByLabel('配对链接').inputValue())
  if (!pairUrl.hash.startsWith('#pair=')) throw new Error('Expected a one-time pairing link')
  await bind(phone, pairUrl.href, '验收手机')
  await checkViewport(phone, 'mobile-dashboard-390')
  await phone.setViewportSize({ width: 375, height: 667 })
  await checkViewport(phone, 'mobile-dashboard-375')
  await phone.setViewportSize({ width: 390, height: 844 })
  await mobile.setOffline(true)
  await wait(text(phone, '正在连接'), 10000)
  const taskHash = await submitFixture(work, '修复空数据导出失败并补回归测试',
    `${managed ? '' : '[ASK_QUESTION] '}修复 src/export_report.sh 在空数据输入时导出失败的问题，保留 CSV 列序并补回归测试。`)
  await work.close()

  await mobile.setOffline(false)
  await wait(text(phone, '实时同步'), 15000)
  await route(phone, 'inbox')
  const request = phone.getByRole('link', { name: managed ? /测试授权/ : /等待答复/ }).first()
  await wait(request, 90000)
  await checkViewport(phone, 'mobile-inbox')
  await request.click()
  if (managed) {
    await wait(phone.getByText('/workspace/tests/export_check.sh', { exact: false }).first())
    await checkViewport(phone, 'mobile-approval')
    await button(phone, '允许本次').click()
    await button(phone, '确认授权').click()
  } else {
    await wait(phone.getByText('空数据导出时是否应保留表头', { exact: false }).first())
    await checkViewport(phone, 'mobile-question')
    await phone.getByRole('textbox', { name: /答复/ }).fill('保留表头，退出码为 0。')
    await button(phone, '提交答复').click()
  }

  await wait(text(phone, 'v1 待验收'), 90000)
  await button(phone, '查看当前版本证据').click()
  if (managed) {
    await phone.getByRole('button', { name: /^实际测试记录 / }).click()
    await wait(phone.locator('.evidence pre').filter({ hasText: '"exitCode": 0' }))
    await phone.getByRole('button', { name: /^测试输出 / }).click()
    await wait(phone.locator('.evidence pre').filter({ hasText: '[export-check] all_checks=pass' }))
  } else {
    await wait(phone.getByRole('button', { name: /^工作树差异 / }))
  }
  await checkViewport(phone, 'mobile-evidence')
  await button(phone, '接受 v1').click()
  await button(phone, '确认接受').click()
  await wait(text(phone, '已完成').first())
  await checkViewport(phone, 'mobile-completed')

  const reopened = await desktop.newPage()
  await reopened.goto(origin + '/' + taskHash)
  await wait(text(reopened, '最近验收'))
  await wait(reopened.getByText('已接受 · 设备', { exact: false }).first())
  await checkViewport(reopened, 'desktop-completed')

  let deniedTaskId
  if (managed) {
    const denied = await submitFixture(reopened, '空数据导出拒绝授权验证',
      '验证手机拒绝授权后，必需证据缺失会阻止验收。')
    deniedTaskId = denied.slice('#task/'.length)
    await route(phone, denied.slice(1))
    await button(phone, '拒绝').click()
    await wait(text(phone, '当前版本缺少验收所需证据。'))
    if (await button(phone, '接受 v1').isEnabled()) throw new Error('Acceptance enabled without required evidence')
  }
  await route(reopened, 'settings')
  const device = reopened.locator('.settings-item').filter({ hasText: '验收手机' })
  await button(device, '撤销').click()
  await button(reopened, '撤销访问').click()
  await wait(phone.getByRole('heading', { name: '连接你的设备' }), 15000)

  const report = {
    status: 'PASS', taskId: taskHash.slice('#task/'.length), deniedTaskId, origin,
    mobileAction: managed ? 'managed-test-approval' : 'answer', layoutChecks,
  }
  const json = JSON.stringify(report, null, 2) + '\n'
  if (options['report-out']) {
    fs.mkdirSync(path.dirname(options['report-out']), { recursive: true })
    fs.writeFileSync(options['report-out'], json)
  }
  process.stdout.write(json)
} finally {
  await browser.close()
}
