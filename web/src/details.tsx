import { ArrowLeft, Check, ClipboardList, Clock3, Download, FileCode2, GitBranch, History, Layers3, Play, RotateCcw, Square, Workflow } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { APIError, apiPath, get, getText, mutate, type Artifact, type Attempt, type Entry, type List, type OwnerRequest, type Project, type Session, type Task, type TimelineEvent } from './api'
import { agentName, date, Empty, eventNames, Loading, Modal, Notice, RequestGlyph, results, short, Status, blockerLabels } from './ui'

type Details = { task: Task, attempts: Attempt[], artifacts: Artifact[], timeline: List<TimelineEvent> }
type Dialog = 'cancel' | 'accept' | 'rework' | undefined
const sections = [['overview', '概况'], ['artifact', '产物与差异'], ['timeline', '时间线'], ['attempts', '尝试记录']]
const evidenceKinds: Record<string, string> = {
  diff: '工作树差异', agent_statement: 'Agent 自述', observed_run: '实际测试记录', dry_run: '模拟执行',
  not_run: '未运行', execution_log: '测试输出', input_manifest: '输入指纹',
}

export function TaskDetails({ taskId, task, sequence, projects, session, online, refresh }: { taskId: string; task?: Task; sequence: string; projects: Project[]; session: Session; online: boolean; refresh: () => Promise<void> }) {
  const [data, setData] = useState<Details>()
  const [error, setError] = useState('')
  const [message, setMessage] = useState('')
  const [busy, setBusy] = useState(false)
  const [section, setSection] = useState('overview')
  const [dialog, setDialog] = useState<Dialog>()
  const [dialogTarget, setDialogTarget] = useState<{ revision: string; artifactId?: string; version?: number }>()
  const [reason, setReason] = useState('')
  const [artifactId, setArtifact] = useState('')

  const load = useCallback(async (signal?: AbortSignal) => {
    try {
      const [task, attempts, artifacts, timeline] = await Promise.all([
        get<Task>('tasks/' + taskId, signal),
        get<List<Attempt>>('tasks/' + taskId + '/attempts', signal),
        get<List<Artifact>>('tasks/' + taskId + '/artifacts', signal),
        get<List<TimelineEvent>>('tasks/' + taskId + '/timeline', signal),
      ])
      if (!signal?.aborted) setData({ task, attempts: attempts.items, artifacts: artifacts.items, timeline })
    } catch (err) {
      if (!signal?.aborted) setError(err instanceof Error ? err.message : '读取任务失败')
    }
  }, [taskId])
  useEffect(() => {
    const controller = new AbortController()
    void load(controller.signal)
    return () => controller.abort()
  }, [load, sequence])
  const current = data?.task ?? task
  const selected = data?.artifacts.find(item => item.artifactVersionId === (artifactId || current?.currentArtifactVersionId)) ?? data?.artifacts[0]
  const activeArtifact = data?.artifacts.find(item => item.artifactVersionId === current?.currentArtifactVersionId)
  const project = projects.find(item => item.projectId === current?.projectId)
  const attempt = data?.attempts.find(item => item.attemptId === current?.currentAttemptId)
  const requests = current?.requests ?? []
  const activeRequests = requests.filter(req => req.status === 'pending')
  const lastAcceptance = data?.timeline.items.filter(event => event.type === 'acceptance_recorded').at(-1)
  const acceptanceAllowed = current?.state === 'awaiting_acceptance' && !!activeArtifact && activeArtifact.integrityStatus === 'available' && (!current.requireTest || activeArtifact.hasRequiredEvidence) && (!current.reviewAgent || activeArtifact.review?.outcome === 'pass')
  const locked = busy || !online || (!!task && task.revision !== current?.revision)
  const ended = current && ['completed', 'failed', 'cancelled'].includes(current.state)

  async function doAction(action: () => Promise<void>) {
    setBusy(true); setError(''); setMessage('')
    try { await action(); await refresh(); await load() }
    catch (err) {
      if (err instanceof APIError && err.code === 'already_decided') {
        const decision = err.details?.decision
        setError(decision ? '已生效 · ' + (results[decision.outcome] ?? decision.outcome) + ' · ' + date(decision.decidedAt) : err.message)
      } else setError(err instanceof Error ? err.message : '操作失败')
      await refresh().catch(() => {}); await load()
    } finally { setBusy(false) }
  }
  if (!current) return error ? <><a className="back" href="#"><ArrowLeft size={16} /> 返回任务总览</a><Notice danger>{error}</Notice></> : <Loading />
  const submitDecision = (request: OwnerRequest, outcome: string, answer?: string) => doAction(async () => {
    await mutate('requests/' + request.requestId + '/decide', {
      taskId, attemptId: request.attemptId, expectedRevision: current.revision,
      inputDigest: request.frozenInput.inputDigest, outcome, answerText: answer,
    }, session)
    setMessage('决定已记录')
  })
  const openDialog = (kind: Dialog) => { setReason(''); setDialogTarget({ revision: current.revision, artifactId: activeArtifact?.artifactVersionId, version: activeArtifact?.versionNumber }); setDialog(kind) }
  const dialogStale = dialogTarget?.revision !== current.revision || (dialog !== 'cancel' && dialogTarget?.artifactId !== activeArtifact?.artifactVersionId)
  const showArtifact = () => { setArtifact(activeArtifact?.artifactVersionId ?? ''); setSection('artifact') }
  return <>
    <a className="back" href="#"><ArrowLeft size={16} /> 返回任务总览</a>
    <div className="page-heading detail-heading">
      <div><div className="detail-kicker"><Status state={current.state} /><span>{project?.name ?? '未选择项目'}</span></div><h1>{current.title || '未命名草稿'}</h1><p>{agentName(current.executionAgent)} 执行{current.reviewAgent ? ' · ' + agentName(current.reviewAgent) + ' 复核' : ''} · 更新于 {date(current.updatedAt)}</p></div>
      <div className="heading-actions">
        {current.state === 'draft' && <a className="button primary" href={'#edit/' + taskId}>编辑草稿</a>}
        {!ended && current.state !== 'awaiting_acceptance' && !current.cancelCommandId && <button className="button secondary" disabled={locked} onClick={() => openDialog('cancel')}><Square size={15} /> 取消任务</button>}
        {(ended || current.blockers.some(block => block.sourceId === 'rework_limit' || block.sourceId === 'restore')) && <button className="button secondary" disabled={locked} onClick={() => void doAction(async () => {
          const next = await mutate<Task>('tasks', { sourceTaskId: taskId }, session)
          await refresh()
          location.hash = 'edit/' + next.taskId
        })}><RotateCcw size={16} /> 另建关联任务</button>}
      </div>
    </div>
    {error && <Notice danger>{error}</Notice>}
    {message && <div className="positive" role="status"><Check size={17} />{message}</div>}
    {current.cancelCommandId && current.state !== 'cancelled' && <Notice>取消请求已记录，等待节点核实进程已停止。</Notice>}
    {current.blockers.filter(block => block.message).map(block => <Notice key={block.sourceId}>{blockerLabels[block.kind]} · {block.message}</Notice>)}
    {activeRequests.map(request => <PendingRequest key={request.requestId} request={request} disabled={locked} decide={(outcome, answer) => void submitDecision(request, outcome, answer)} />)}
    {current.state === 'awaiting_acceptance' && <div className="review-callout">
      <div className="callout-icon"><ClipboardList size={21} /></div>
      <div><strong>{activeArtifact ? 'v' + activeArtifact.versionNumber + ' 待验收' : '当前版本待验收'}</strong><p>{activeArtifact?.review ? '独立复核已记录。核对差异和验证结果后作出决定。' : '核对本次差异、测试证据与验收条件。'}</p>{activeArtifact && <button className="text-button" onClick={showArtifact}>查看当前版本证据</button>}</div>
      <div className="callout-actions"><button className="button secondary" disabled={locked} onClick={() => openDialog('rework')}>要求返工</button><button className="button primary" disabled={locked || !acceptanceAllowed} onClick={() => openDialog('accept')}><Check size={16} /> 接受{activeArtifact ? ' v' + activeArtifact.versionNumber : '版本'}</button></div>
    </div>}
    {current.state === 'awaiting_acceptance' && activeArtifact && !acceptanceAllowed && <Notice>{activeArtifact.integrityStatus !== 'available' ? '当前归档缺失或校验失败。' : '当前版本缺少验收所需证据。'}</Notice>}
    {current.state === 'awaiting_acceptance' && <div className="detail-link-row"><button className="text-button" disabled={locked} onClick={() => openDialog('cancel')}>取消任务</button></div>}
    {attempt?.recoveryOptions.includes('new_attempt') && !ended && <div className="detail-link-row"><button className="button secondary" disabled={locked} onClick={() => void doAction(async () => {
      await mutate('tasks/' + taskId + '/recover', { attemptId: attempt.attemptId, expectedRevision: current.revision, choice: 'new_attempt', evidenceRefs: [] }, session)
    })}><Play size={16} /> 开启新尝试</button></div>}
    <section className="panel details-panel">
      <div className="tabs detail-tabs" aria-label="任务内容">
        {sections.map(([id, name]) => <button className={id === section ? 'selected' : ''} key={id} onClick={() => setSection(id)}>{name}</button>)}
      </div>
      {section === 'overview' && <div className="overview">
        <div className="requirements">
          <Info title="目标">{current.goal || '未填写'}</Info>
          <Info title="范围">{current.scope || '未填写'}</Info>
          <Info title="验收条件">{current.acceptanceCriteria || '未填写'}</Info>
          {current.feedback && <Info title="返工原因">{current.feedback}</Info>}
        </div>
        <div className="detail-facts">
          <Info title="基准提交"><code>{short(current.baseCommit)}</code></Info>
          <Info title="当前尝试">{attempt ? '#' + attempt.ordinal + ' · ' + (results[attempt.status] ?? attempt.status) : '—'}</Info>
          <Info title="返工轮数">{current.reworkCount} / 2</Info>
          {current.sourceTaskId && <Info title="来源任务"><a href={'#task/' + current.sourceTaskId}>{short(current.sourceTaskId)}</a></Info>}
          {lastAcceptance && <Info title="最近验收">{results[String(lastAcceptance.data?.outcome)]} · 设备 {short(String(lastAcceptance.data?.deviceId ?? ''))} · {date(lastAcceptance.occurredAt)}</Info>}
        </div>
        {requests.filter(req => req.status !== 'pending').length > 0 && <div className="request-history">
          <h3>请求记录</h3>
          {requests.filter(req => req.status !== 'pending').map(req => <div className="settings-item" key={req.requestId}>
            <RequestGlyph kind={req.kind} /><div><strong>{req.kind === 'action_approval' ? req.frozenInput.label || '测试授权' : req.frozenInput.question}</strong><span>{req.decision ? (results[req.decision.outcome] || req.decision.outcome) + ' · ' + date(req.decision.decidedAt) + (req.decision.deviceId ? ' · 设备 ' + short(req.decision.deviceId) : '') : '已关闭'}{req.result ? ' · ' + (results[req.result] || req.result) : ''}</span>{req.decision?.answerText && <span>{req.decision.answerText}</span>}</div>
          </div>)}
        </div>}
      </div>}
      {section === 'artifact' && (selected ? <ArtifactView artifact={selected} options={data?.artifacts ?? []} select={setArtifact} /> : <Empty title="尚无归档产物" icon={<Layers3 size={23} />} />)}
      {section === 'timeline' && (data ? <div className="timeline">
        {data.timeline.items.slice().reverse().map(event => <div className="timeline-item" key={event.seq}><div className="timeline-icon"><History size={15} /></div><div><strong>{eventNames[event.type] ?? event.type}</strong><span>{[event.data?.outcome && (results[String(event.data.outcome)] ?? String(event.data.outcome)), event.data?.phase && (results[String(event.data.phase)] ?? String(event.data.phase)), event.data?.deviceId && '设备 ' + short(String(event.data.deviceId)), event.data?.artifactVersionId && '版本 ' + short(String(event.data.artifactVersionId))].filter(Boolean).join(' · ')}</span></div><time>{date(event.occurredAt)}</time></div>)}
        {data.timeline.nextCursor && <button className="button secondary" onClick={() => void get<List<TimelineEvent>>('tasks/' + taskId + '/timeline?cursor=' + data.timeline.nextCursor).then(page => setData({ ...data, timeline: { items: [...data.timeline.items, ...page.items], nextCursor: page.nextCursor } })).catch(err => setError(err.message))}>加载更多记录</button>}
      </div> : <Loading />)}
      {section === 'attempts' && (data?.attempts.length ? <div className="attempt-list">
        {data.attempts.slice().reverse().map(item => <div className="settings-item" key={item.attemptId}><Workflow size={19} /><div><strong>尝试 #{item.ordinal} · {agentName(item.agent)} {item.role === 'review' ? '复核' : '执行'}</strong><span>{date(item.startedAt)}{item.endedAt ? ' – ' + date(item.endedAt) : ''} · {short(item.attemptId)}</span>{item.nativeSessionId && <span>原生会话 {short(item.nativeSessionId)}</span>}{item.result && <span>{results[item.result] ?? item.result}</span>}</div><span className="mini">{results[item.status] ?? item.status}</span></div>)}
      </div> : <Empty title="尚无执行或复核尝试" icon={<Clock3 size={23} />} />)}
    </section>
    {dialog && <Modal title={dialog === 'cancel' ? '取消任务' : dialog === 'accept' ? '接受 v' + dialogTarget?.version : '要求返工'} onClose={() => setDialog(undefined)}>
      {dialog === 'accept' ? <p>确认你已核对 v{dialogTarget?.version} 的证据。接受后此任务将记录为已完成。</p> : <label>{dialog === 'cancel' ? '取消原因（可选）' : '返工原因'}<textarea rows={3} value={reason} onChange={e => setReason(e.target.value)} placeholder={dialog === 'rework' ? '说明需要补充的证据或修改' : '记录取消缘由'} /></label>}
      {dialogStale && <Notice>任务已有更新，请返回并查看当前状态。</Notice>}
      <div className="modal-actions"><button className="button secondary" onClick={() => setDialog(undefined)}>返回</button><button className={'button ' + (dialog === 'cancel' ? 'destructive' : 'primary')} disabled={locked || dialogStale || (dialog === 'rework' && !reason.trim())} onClick={() => {
        const choice = dialog
        setDialog(undefined)
        void doAction(async () => {
          if (choice === 'cancel') await mutate('tasks/' + taskId + '/cancel', { expectedRevision: dialogTarget?.revision, reason }, session)
          else await mutate('tasks/' + taskId + '/acceptance', { expectedRevision: dialogTarget?.revision, artifactVersionId: dialogTarget?.artifactId, outcome: choice === 'accept' ? 'accept' : 'request_rework', reason }, session)
          setMessage('决定已记录')
        })
      }}>{dialog === 'cancel' ? '确认取消' : dialog === 'accept' ? '确认接受' : '提交返工'}</button></div>
    </Modal>}
  </>
}
function Info({ title, children }: { title: string; children: React.ReactNode }) { return <div className="info"><h3>{title}</h3><div>{children}</div></div> }

function PendingRequest({ request, decide, disabled }: { request: OwnerRequest; decide: (outcome: string, answer?: string) => void; disabled: boolean }) {
  const [answer, setAnswer] = useState('')
  const [confirm, setConfirm] = useState(false)
  const approval = request.kind === 'action_approval'
  const frozen = request.frozenInput
  return <section className="request-card">
    <div className="request-title"><div className="callout-icon"><RequestGlyph kind={request.kind} /></div><div><h2>{approval ? frozen.label ?? '受管测试授权' : 'Agent 等待答复'}</h2><p>尝试 {short(request.attemptId)} · 到期 {date(request.expiresAt)}</p></div></div>
    {approval ? <>
      <dl className="request-facts"><div><dt>登记命令</dt><dd><code>{frozen.args?.join(' ')}</code></dd></div><div><dt>工作区</dt><dd><code>{short(frozen.workspaceId)}</code></dd></div><div><dt>输入指纹</dt><dd><code>{frozen.snapshotHash}</code></dd></div><div><dt>执行范围</dt><dd>独立容器 · 只读输入 · 无网络 · {frozen.timeoutSeconds} 秒超时</dd></div></dl>
      <div className="callout-actions"><button className="button secondary" disabled={disabled} onClick={() => decide('deny')}>拒绝</button><button className="button primary" disabled={disabled} onClick={() => setConfirm(true)}>允许本次</button></div>
      {confirm && <Modal title="允许本次测试" onClose={() => setConfirm(false)}><p>允许在已冻结的输入上执行一次「{frozen.label}」。</p><div className="modal-actions"><button className="button secondary" onClick={() => setConfirm(false)}>返回</button><button className="button primary" disabled={disabled} onClick={() => { setConfirm(false); decide('allow_once') }}>确认授权</button></div></Modal>}
    </> : <form onSubmit={event => { event.preventDefault(); if (answer.trim()) decide('answer', answer.trim()) }}><p className="question">{frozen.question}</p><label>答复<textarea value={answer} onChange={event => setAnswer(event.target.value)} required rows={3} /></label><button className="button primary" disabled={disabled || !answer.trim()}>提交答复</button></form>}
  </section>
}

function ArtifactView({ artifact, options, select }: { artifact: Artifact; options: Artifact[]; select: (id: string) => void }) {
  const [entry, setEntry] = useState<Entry>()
  const [text, setText] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const selected = entry && artifact.manifest.find(item => item.entryId === entry.entryId)
  const path = selected ? 'artifacts/' + artifact.artifactVersionId + '/entries/' + selected.entryId : ''
  useEffect(() => {
    if (!path) return
    const abort = new AbortController()
    setText(''); setError(''); setLoading(true)
    getText(path, abort.signal).then(setText).catch(err => { if (!abort.signal.aborted) setError(err.message) }).finally(() => { if (!abort.signal.aborted) setLoading(false) })
    return () => abort.abort()
  }, [path])
  return <div className="artifact-view">
    <div className="artifact-top"><div><div className="artifact-label"><GitBranch size={17} /><h3>归档版本</h3></div><code>{artifact.baseCommit}</code></div>
      <select aria-label="归档版本" value={artifact.artifactVersionId} onChange={event => { setEntry(undefined); select(event.target.value) }}>{options.map(item => <option value={item.artifactVersionId} key={item.artifactVersionId}>v{item.versionNumber}</option>)}</select></div>
    {artifact.integrityStatus !== 'available' && <Notice danger>归档条目缺失或校验失败。该版本暂时无法接受。</Notice>}
    {artifact.review && <div className="review-note"><strong>{results[artifact.review.outcome] ?? artifact.review.outcome}</strong><span>{artifact.review.reason}</span>{artifact.review.evidenceRefs?.length ? <small>证据 · {artifact.review.evidenceRefs.join('、')}</small> : null}</div>}
    {artifact.acceptance && <div className="review-note"><strong>{results[artifact.acceptance.outcome] ?? artifact.acceptance.outcome} · {date(artifact.acceptance.decidedAt)}</strong><span>设备 {short(artifact.acceptance.deviceId)}</span>{artifact.acceptance.reason && <span>{artifact.acceptance.reason}</span>}</div>}
    <div className="artifact-entries">{artifact.manifest.map(item => <button type="button" key={item.entryId} className={'entry-button' + (item.entryId === selected?.entryId ? ' active' : '')} onClick={() => setEntry(item)}><FileCode2 size={17} /><span><strong>{evidenceKinds[item.evidenceKind] ?? item.entryId}</strong><small>{item.entryId} · {item.size.toLocaleString()} B</small></span></button>)}</div>
    {selected && <div className="evidence"><div><strong>{selected.entryId}</strong><a className="button quiet" href={apiPath(path)} download={selected.entryId}><Download size={16} /> 下载</a></div>{loading ? <Loading /> : error ? <Notice danger>{error}</Notice> : <pre>{text || '该条目没有内容'}</pre>}</div>}
  </div>
}
