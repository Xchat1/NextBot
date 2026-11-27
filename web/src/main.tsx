import { ArrowRight, Bell, ChevronRight, Command, Download, GitBranch, Laptop, LayoutDashboard, Monitor, Plus, Search, Server, Settings, ShieldCheck, Smartphone, Workflow } from 'lucide-react'
import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react'
import { createRoot } from 'react-dom/client'
import { APIError, followEvents, get, mutate, replay, snapshot, type Device, type List, type Session, type Snapshot, type Task } from './api'
import { TaskDetails } from './details'
import { TaskForm } from './form'
import { agentName, Brand, date, Dot, Empty, Loading, MetricIcons, Modal, Notice, RequestGlyph, TaskRow } from './ui'
import './style.css'

const desktopClient = (window as Window & { nextBotDesktop?: boolean }).nextBotDesktop === true

function currentRoute() {
  const value = location.hash.slice(1)
  return /^(new|settings|inbox|task\/[a-f0-9]{32}|edit\/[a-f0-9]{32})$/.test(value) ? value : ''
}
function takeTicket() {
  const params = new URLSearchParams(location.hash.slice(1))
  const claim = params.get('claim')
  const pair = params.get('pair')
  if (claim || pair) history.replaceState(null, '', location.pathname + location.search)
  return { value: claim || pair || '', mode: pair ? 'pair' : 'claim' }
}

function App() {
  const [ticket] = useState(takeTicket)
  const [route, setRoute] = useState(currentRoute)
  const [session, setSession] = useState<Session | null | undefined>(undefined)
  const [snapshotData, setSnapshotData] = useState<Snapshot>()
  const [online, setOnline] = useState(false)
  const [error, setError] = useState('')
  const [initKey, setInitKey] = useState(0)
  const refreshData = useRef<() => Promise<void>>(async () => {})

  useEffect(() => {
    const listener = () => { setRoute(currentRoute()); window.scrollTo(0, 0) }
    window.addEventListener('hashchange', listener)
    return () => window.removeEventListener('hashchange', listener)
  }, [])
  useEffect(() => {
    const request = new AbortController()
    get<Session>('session', request.signal).then(value => { setError(''); setSession(value) }).catch(err => {
      if (request.signal.aborted) return
      if (err instanceof APIError && err.status === 401) { setError(''); setSession(null) }
      else { setError(err.message); setSession(null) }
    })
    return () => request.abort()
  }, [initKey])
  useEffect(() => {
    if (!session) return
    const root = new AbortController()
    let position: Snapshot | undefined
    let refreshing: Promise<void> | undefined
    let refreshWanted = 0
    let refreshApplied = 0
    let refreshTimer: number | undefined
    let attemptController: AbortController | undefined
    let wakeWait: (() => void) | undefined
    let lastPacketAt = Date.now()
    const fail = (err: unknown) => {
      if (root.signal.aborted) return
      if (err instanceof APIError && err.status === 401) { setSession(null); setSnapshotData(undefined); return }
      setError(err instanceof Error ? err.message : '同步失败')
    }
    const refresh = () => {
      refreshWanted++
      refreshing ??= (async () => {
        while (refreshApplied < refreshWanted && !root.signal.aborted) {
          const request = refreshWanted
          const result = await snapshot(root.signal)
          position = result
          refreshApplied = request
          setSnapshotData(result)
          setError('')
        }
      })().finally(() => { refreshing = undefined })
      return refreshing
    }
    refreshData.current = refresh
    const wait = (ms: number) => new Promise<void>(resolve => {
      let timer: number | undefined
      const done = () => {
        window.clearTimeout(timer)
        root.signal.removeEventListener('abort', done)
        if (wakeWait === done) wakeWait = undefined
        resolve()
      }
      wakeWait = done
      root.signal.addEventListener('abort', done, { once: true })
      if (ms >= 0) timer = window.setTimeout(done, ms)
    })
    const onOffline = () => {
      setOnline(false)
      attemptController?.abort(new DOMException('Network offline', 'AbortError'))
    }
    const onOnline = () => { wakeWait?.() }
    const onVisibility = () => {
      if (document.visibilityState === 'visible' && Date.now() - lastPacketAt > 12_000) {
        setOnline(false)
        attemptController?.abort(new DOMException('Stale connection after backgrounding', 'AbortError'))
        wakeWait?.()
      }
    }
    window.addEventListener('offline', onOffline)
    window.addEventListener('online', onOnline)
    document.addEventListener('visibilitychange', onVisibility)
    void (async () => {
      while (!root.signal.aborted) {
        if (!navigator.onLine) {
          setOnline(false)
          await wait(-1)
          continue
        }
        attemptController = new AbortController()
        const forwardAbort = () => attemptController?.abort(root.signal.reason)
        root.signal.addEventListener('abort', forwardAbort, { once: true })
        try {
          const device = await get<Session>('session', attemptController.signal)
          if (device.deviceId !== session.deviceId || device.instanceId !== session.instanceId ||
              device.csrfToken !== session.csrfToken || device.requireReview !== session.requireReview ||
              device.pairingOrigin !== session.pairingOrigin) {
            setSession(device)
            return
          }
          await replay(session)
          if (!position) await refresh()
          if (!position || root.signal.aborted) return
          await followEvents(
            position.streamId,
            position.highWater,
            attemptController.signal,
            () => { lastPacketAt = Date.now(); setOnline(true); setError('') },
            () => {
              if (refreshTimer === undefined) refreshTimer = window.setTimeout(() => {
                refreshTimer = undefined
                void refresh().catch(fail)
              }, 120)
            },
            () => { lastPacketAt = Date.now() },
          )
        } catch (err) {
          if (root.signal.aborted) return
          const intentional = attemptController.signal.aborted && (!navigator.onLine || (err instanceof DOMException && err.name === 'AbortError'))
          if (err instanceof APIError && err.status === 410) position = undefined
          else if (!intentional) fail(err)
          if (err instanceof APIError && err.status === 401) return
        } finally {
          root.signal.removeEventListener('abort', forwardAbort)
          attemptController = undefined
        }
        setOnline(false)
        if (!root.signal.aborted && navigator.onLine) await wait(1200)
      }
    })()
    return () => {
      root.abort()
      window.clearTimeout(refreshTimer)
      window.removeEventListener('offline', onOffline)
      window.removeEventListener('online', onOnline)
      document.removeEventListener('visibilitychange', onVisibility)
      setOnline(false)
    }
  }, [session])

  const refresh = useCallback(async () => {
    try { await refreshData.current() } catch (err) { if (err instanceof APIError && err.status === 401) { setSession(null); setSnapshotData(undefined) } else throw err }
  }, [])
  const authSuccess = async () => {
    try { setSession(await get<Session>('session')); setError('') } catch (err) { setError(err instanceof Error ? err.message : '会话不可用') }
  }

  if (session === undefined) return <div className="initial"><Brand /><Loading /></div>
  if (!session) return <Bind ticket={ticket} onSuccess={authSuccess} error={error} onRetry={() => { setSession(undefined); setInitKey(value => value + 1) }} />
  const snapshotState = snapshotData
  const tasks = snapshotState?.tasks ?? []
  const pending = snapshotState?.pendingRequests ?? []
  const approvals = tasks.filter(task => task.state === 'awaiting_acceptance')
  const inboxCount = pending.length + approvals.length
  const nodeOnline = snapshotState?.nodes.some(node => node.availability === 'online') ?? false
  const taskID = route.split('/')[1]
  const activeTask = tasks.find(task => task.taskId === taskID)
  const heading = route === 'settings' ? '设置' : route === 'inbox' ? '待我处理' : route === 'new' ? '新建任务' : route.startsWith('edit/') ? '编辑草稿' : route.startsWith('task/') ? '任务详情' : '任务总览'
  return <div className="shell">
    <aside className="sidebar">
      <a className="home-link" href="#" aria-label="NextBot 任务总览"><Brand /></a>
      <div className="sidebar-caption">工作区</div>
      <nav aria-label="主导航">
        <a className={!route || route.startsWith('task/') || route.startsWith('edit/') ? 'active' : ''} href="#"><LayoutDashboard size={18} /> <span>任务总览</span></a>
        <a className={route === 'inbox' ? 'active' : ''} href="#inbox"><Bell size={18} /> <span>待我处理</span>{inboxCount > 0 && <b className="nav-count">{inboxCount}</b>}</a>
        <a className={route === 'settings' ? 'active' : ''} href="#settings"><Settings size={18} /> <span>设置</span></a>
      </nav>
      <div className="host-card">
        <div className="host-icon"><Server size={17} /></div>
        <div><strong>本地执行节点</strong><span><i className={nodeOnline ? 'up' : ''} />{nodeOnline ? '节点在线' : '节点离线'}</span></div>
        <a href="#settings" className="icon-button" aria-label="查看节点"><ChevronRight size={16} /></a>
      </div>
    </aside>
    <div className="main-shell">
      <header className="topbar">
        <a className="mobile-brand" href="#" aria-label="NextBot 任务总览"><Brand /></a>
        <div className="breadcrumbs"><span>控制台</span><ChevronRight size={14} /><strong>{heading}</strong></div>
        <div className="topbar-actions">
          <span className={online ? 'sync online' : 'sync'}><Dot />{online ? '实时同步' : '正在连接'}</span>
          <a href="#inbox" className="icon-button top-bell" aria-label="待我处理"><Bell size={18} />{inboxCount > 0 && <i />}</a>
          <a href="#settings" className="avatar" aria-label="设备设置">{session.deviceId.slice(0, 2).toUpperCase()}</a>
        </div>
      </header>
      <main className="content" id="main-content">
        {error && <Notice danger>{error}</Notice>}
        {!snapshotState ? <Loading /> : route === 'settings' ? <SettingsPage session={session} snapshot={snapshotState} refresh={refresh} signOut={() => { setSession(null); setSnapshotData(undefined) }} />
          : route === 'new' || route.startsWith('edit/') ? <TaskForm key={taskID || 'new'} task={activeTask} snapshot={snapshotState} session={session} online={online} refresh={refresh} />
          : route.startsWith('task/') ? <TaskDetails key={taskID} taskId={taskID} task={activeTask} projects={snapshotState.projects} session={session} online={online} refresh={refresh} sequence={snapshotState.highWater} />
          : route === 'inbox' ? <Inbox tasks={tasks} pending={pending} approvals={approvals} />
          : <Dashboard snapshot={snapshotState} inboxCount={inboxCount} />}
      </main>
    </div>
  </div>
}

function Bind({ ticket, onSuccess, error, onRetry }: { ticket: { value: string; mode: string }; onSuccess: () => void; error: string; onRetry: () => void }) {
  const [mode, setMode] = useState(ticket.mode)
  const [value, setValue] = useState(ticket.value)
  const [name, setName] = useState(desktopClient ? 'Mac' : '')
  const [err, setError] = useState('')
  const [busy, setBusy] = useState(false)
  async function submit(event: FormEvent) {
    event.preventDefault()
    setBusy(true); setError('')
    try {
      await mutate(mode === 'claim' ? 'owner/claim' : 'pairings/claim', { [mode === 'claim' ? 'ticket' : 'token']: value.trim(), deviceName: name.trim() })
      onSuccess()
    } catch (e) {
      const current = await get<Session>('session').catch(() => null)
      if (current) onSuccess()
      else setError(e instanceof Error ? e.message : '绑定失败')
    } finally { setBusy(false) }
  }
  return <div className="bind-layout">
    <section className="bind-intro">
      <Brand />
      <div className="bind-feature"><div className="bind-feature-icon"><Workflow size={31} /></div><h1>{desktopClient ? <>在这台 Mac 上<br />管理 Agent 任务。</> : <>在电脑上派单，<br />在手机上继续。</>}</h1><p>跟踪本地 Agent 执行，处理授权，并根据归档证据验收交付。</p></div>
      <div className="bind-footer"><ShieldCheck size={16} /> {desktopClient ? '本机实例 · 设备绑定' : '私网实例 · 设备绑定'}</div>
    </section>
    <section className="bind-main">
      <form className="bind-card" onSubmit={submit}>
        <div className="bind-lock"><Laptop size={24} /></div>
        <h2>{desktopClient ? '设置这台 Mac' : '连接你的设备'}</h2>
        <p>{desktopClient ? (value ? '确认设备名称，开始使用本机控制台。' : '请从「工作区」菜单重新绑定这台 Mac。') : '使用安装主机签发的认领票据，或使用可信设备生成的配对凭据。'}</p>
        {!desktopClient && <div className="segmented" aria-label="绑定方式">
          <button type="button" className={mode === 'claim' ? 'selected' : ''} onClick={() => setMode('claim')}>所有者认领</button>
          <button type="button" className={mode === 'pair' ? 'selected' : ''} onClick={() => setMode('pair')}>设备配对</button>
        </div>}
        <label>设备名称<input autoComplete="off" required maxLength={100} value={name} onChange={event => setName(event.target.value)} placeholder="例如，办公室电脑" /></label>
        {!desktopClient && <label>{mode === 'claim' ? '认领票据' : '配对凭据'}<input type="password" autoComplete="off" required value={value} onChange={event => setValue(event.target.value)} placeholder="输入一次性凭据" /></label>}
        {(err || error) && <Notice danger>{err || error}</Notice>}
        <button className="button primary full" disabled={busy || (desktopClient && !value)}>{busy ? '正在绑定…' : (desktopClient ? '开始使用' : '绑定此设备')}<ArrowRight size={17} /></button>
        {!desktopClient && mode === 'claim' && <div className="bind-help"><Command size={14} /> 安装主机执行 <code>nextbot claim</code></div>}
        {error && <button type="button" className="button quiet" onClick={onRetry}>重新连接</button>}
      </form>
    </section>
  </div>
}

function Dashboard({ snapshot, inboxCount }: { snapshot: Snapshot; inboxCount: number }) {
  const [filter, setFilter] = useState('all')
  const [search, setSearch] = useState('')
  const { tasks, projects } = snapshot
  const running = tasks.filter(task => ['queued', 'running', 'awaiting_review', 'reviewing'].includes(task.state)).length
  const done = tasks.filter(task => task.state === 'completed').length
  const needsOwner = new Set([...snapshot.pendingRequests.map(req => req.taskId), ...tasks.filter(task => task.state === 'awaiting_acceptance' || task.blockers.some(blocker => blocker.message && blocker.kind === 'awaiting_input')).map(task => task.taskId)])
  const filtered = useMemo(() => {
    const term = search.toLocaleLowerCase().trim()
    return tasks.filter(task => {
      const matches = filter === 'all' || (filter === 'todo' && needsOwner.has(task.taskId)) || (filter === 'draft' && task.state === 'draft') || (filter === 'finished' && ['completed', 'cancelled', 'failed'].includes(task.state))
      return matches && (!term || [task.title, task.goal, projects.find(project => project.projectId === task.projectId)?.name ?? ''].some(text => text.toLocaleLowerCase().includes(term)))
    }).sort((a, b) => Number(needsOwner.has(b.taskId)) - Number(needsOwner.has(a.taskId)))
  }, [tasks, projects, filter, search, needsOwner])
  const metrics = [['全部任务', tasks.length], ['执行与复核', running], ['待我处理', inboxCount], ['已完成', done]]
  return <>
    <div className="page-heading"><div><h1>任务总览</h1><p>查看执行进度，处理授权并验收产物。</p></div><a className="button primary" href="#new"><Plus size={18} /> 新建任务</a></div>
    <section className="metrics" aria-label="任务统计">
      {metrics.map(([label, number], index) => { const Icon = MetricIcons[index]; return <div className={'metric metric-' + index} key={label}><span className="metric-icon"><Icon size={18} /></span><div><strong>{number}</strong><span>{label}</span></div></div> })}
    </section>
    <section className="panel task-panel">
      <div className="panel-heading"><div><h2>任务列表</h2><span>{filtered.length} 项任务</span></div><label className="search"><Search size={16} /><input aria-label="搜索任务" value={search} onChange={event => setSearch(event.target.value)} placeholder="搜索任务…" /></label></div>
      <div className="tabs" aria-label="筛选任务">
        {[['all', '全部'], ['todo', '待我处理'], ['draft', '草稿'], ['finished', '已结束']].map(([key, text]) => <button key={key} className={filter === key ? 'selected' : ''} onClick={() => setFilter(key)}>{text}</button>)}
      </div>
      {filtered.length > 0 ? <div className="task-list">{filtered.map(task => <TaskRow key={task.taskId} task={task} project={projects.find(project => project.projectId === task.projectId)?.name} />)}</div>
        : <Empty title={tasks.length > 0 ? '没有匹配的任务' : '还没有任务'}>{tasks.length === 0 && <><p>选择仓库和基准提交，创建你的第一项任务。</p><a className="button secondary" href="#new"><Plus size={16} /> 新建任务</a></>}</Empty>}
    </section>
  </>
}

function Inbox({ tasks, pending, approvals }: { tasks: Task[]; pending: Snapshot['pendingRequests']; approvals: Task[] }) {
  const manual = tasks.filter(task => task.state !== 'awaiting_acceptance' && task.blockers.some(blocker => blocker.kind === 'awaiting_input' && blocker.message))
  return <>
    <div className="page-heading"><div><h1>待我处理</h1><p>处理 Agent 的请求、待核对事项和版本验收。</p></div></div>
    <section className="panel inbox-panel">
      {pending.length + approvals.length + manual.length === 0 ? <Empty title="当前没有待办" icon={<Bell size={23} />} /> : <>
        {pending.map(request => <a className="task-row" key={request.requestId} href={'#task/' + request.taskId}>
          <div className="task-glyph awaiting_acceptance"><RequestGlyph kind={request.kind} /></div>
          <div className="task-primary"><strong>{request.kind === 'action_approval' ? '测试授权' : '等待答复'}</strong><span>{tasks.find(task => task.taskId === request.taskId)?.title ?? '任务'} · 到期 {date(request.expiresAt)}</span></div><ChevronRight size={17} className="row-arrow" />
        </a>)}
        {approvals.map(task => <TaskRow key={task.taskId} task={task} label="核对并验收当前版本" />)}
        {manual.map(task => <TaskRow key={task.taskId} task={task} label="需要核对或恢复" />)}
      </>}
    </section>
  </>
}

function SettingsPage({ session, snapshot, refresh, signOut }: { session: Session; snapshot: Snapshot; refresh: () => Promise<void>; signOut: () => void }) {
  const [devices, setDevices] = useState<Device[]>()
  const [error, setError] = useState('')
  const [pair, setPair] = useState<{ token: string; expiresAt: string }>()
  const [confirm, setConfirm] = useState<Device>()
  const [busy, setBusy] = useState(false)
  const [copied, setCopied] = useState(false)
  const load = useCallback(async () => {
    try { setDevices((await get<List<Device>>('devices')).items) } catch (err) { setError(err instanceof Error ? err.message : '设备读取失败') }
  }, [])
  useEffect(() => { void load() }, [load, snapshot.highWater])
  const pairingOrigin = session.pairingOrigin || (desktopClient ? '' : location.origin)
  const pairUrl = pair ? pairingOrigin + '/#pair=' + encodeURIComponent(pair.token) : ''

  async function action(fn: () => Promise<void>) {
    setBusy(true); setError('')
    try { await fn() } catch (err) { setError(err instanceof Error ? err.message : '操作失败') } finally { setBusy(false) }
  }
  return <div className="settings-layout">
    <div className="page-heading"><div><h1>设置</h1><p>管理执行节点、允许访问的仓库和可信设备。</p></div></div>
    {error && <Notice danger>{error}</Notice>}
    <section className="panel">
      <div className="panel-heading"><div><h2>执行节点</h2><span>连接状态与 Agent 能力</span></div><Server size={20} /></div>
      {snapshot.nodes.length === 0 ? <Empty title="未连接执行节点"><p>在安装主机启动 NextBot Runner。</p></Empty> : snapshot.nodes.map(node => <div className="settings-body" key={node.nodeId}>
        <div className="settings-item"><Server size={19} /><div><strong>{node.nodeId === 'local' ? '本地节点' : node.nodeId}</strong><span>最近联系 {date(node.lastSeenAt)}</span></div><span className={node.availability === 'online' ? 'mini success' : 'mini'}>{node.availability === 'online' ? '在线' : '离线'}</span></div>
        {Object.entries(node.adapterCapabilities).map(([agent, cap]) => <div className="settings-item" key={agent}>
          <Workflow size={19} /><div><strong>{agentName(agent)}</strong><span>{cap.version || '版本未知'} · {cap.authenticated ? '已登录' : '未登录'}</span>{cap.reason && <span>{cap.reason}</span>}</div>
          <span className={cap.execution || cap.readOnlyReview ? 'mini success' : 'mini'}>{cap.execution ? (cap.managedTests ? '执行及受管测试' : '受限文件执行') : cap.readOnlyReview ? '只读复核' : '待验证'}</span>
        </div>)}
      </div>)}
    </section>
    <section className="panel">
      <div className="panel-heading"><div><h2>登记仓库</h2><span>由安装主机管理仓库与受管测试</span></div><GitBranch size={20} /></div>
      {snapshot.projects.length === 0 ? <Empty title="尚未登记仓库"><code>nextbot project-add --repo /path/to/repo</code></Empty> : <div className="settings-body">{snapshot.projects.map(project => <div className="settings-item" key={project.projectId}><GitBranch size={18} /><div><strong>{project.name}</strong><span>{project.tests?.length ?? 0} 项登记测试</span></div></div>)}</div>}
    </section>
    <section className="panel">
      <div className="panel-heading"><div><h2>可信设备</h2><span>{pairingOrigin ? '撤销设备后，现有会话将失效。' : '在 Mac 工具栏的「手机接手」中设置私网 HTTPS 地址后，即可配对手机。'}</span></div>{pairingOrigin && <button className="button secondary" disabled={busy} onClick={() => void action(async () => { setCopied(false); setPair(await mutate('pairings', {}, session)) })}><Plus size={17} /> 配对设备</button>}</div>
      {devices ? <div className="settings-body">{devices.map(device => <div className="settings-item" key={device.deviceId}>
        {device.isCurrent ? <Monitor size={19} /> : <Smartphone size={19} />}<div><strong>{device.deviceName}</strong><span>绑定于 {date(device.boundAt)}</span></div>
        {device.isCurrent && <span className="mini success">当前设备</span>}
        {device.revokedAt ? <span className="mini">已撤销</span> : <button className="button quiet" onClick={() => setConfirm(device)}>撤销</button>}
      </div>)}</div> : <Loading />}
    </section>
    <section className="panel">
      <div className="panel-heading"><div><h2>运行诊断</h2><span>导出实例、任务和节点标识及状态</span></div><button type="button" className="button secondary" disabled={busy} onClick={() => void action(async () => {
        const result = await get('diagnostics')
        const url = URL.createObjectURL(new Blob([JSON.stringify(result, null, 2)], { type: 'application/json' }))
        const link = document.createElement('a'); link.href = url; link.download = 'nextbot-diagnostics.json'; link.click()
        window.setTimeout(() => URL.revokeObjectURL(url), 1000)
      })}><Download size={17} /> 导出诊断</button></div>
    </section>
    {pair && <Modal title="配对新设备" onClose={() => setPair(undefined)}>
      <p>在新设备的浏览器中打开此链接，然后填写设备名称。凭据在 {date(pair.expiresAt)} 到期。</p>
      <label>配对链接<input readOnly value={pairUrl} onFocus={event => event.target.select()} /></label>
      <div className="modal-actions"><button className="button secondary" onClick={() => setPair(undefined)}>关闭</button><button className="button primary" onClick={() => void action(async () => { await navigator.clipboard.writeText(pairUrl); setCopied(true) })}>{copied ? '已复制' : '复制链接'}</button></div>
    </Modal>}
    {confirm && <Modal title="撤销设备" onClose={() => setConfirm(undefined)}>
      <p>撤销「{confirm.deviceName}」后，该设备将立即失去任务和决定的访问权限。</p>
      <div className="modal-actions"><button className="button secondary" onClick={() => setConfirm(undefined)}>返回</button><button className="button destructive" disabled={busy} onClick={() => void action(async () => {
        await mutate('devices/' + confirm.deviceId + '/revoke', {}, session)
        if (confirm.isCurrent) signOut()
        else { setConfirm(undefined); await load(); await refresh() }
      })}>撤销访问</button></div>
    </Modal>}
  </div>
}
const root = document.getElementById('root')!
createRoot(root).render(<App />)
