import { ArrowLeft, ArrowRight, GitBranch, Save, Workflow } from 'lucide-react'
import { useState, type FormEvent } from 'react'
import { get, mutate, type Resolution, type Session, type Snapshot, type Task } from './api'
import { Notice } from './ui'

export function TaskForm({ task, session, snapshot, online, refresh }: { task?: Task; session: Session; snapshot: Snapshot; online: boolean; refresh: () => Promise<void> }) {
  const [projectId, setProject] = useState(task?.projectId || snapshot.projects[0]?.projectId || '')
  const [title, setTitle] = useState(task?.title ?? '')
  const [goal, setGoal] = useState(task?.goal ?? '')
  const [scope, setScope] = useState(task?.scope ?? '')
  const [criteria, setCriteria] = useState(task?.acceptanceCriteria ?? '')
  const [ref, setRef] = useState(task?.refName || 'HEAD')
  const [execution, setExecution] = useState(task?.executionAgent || 'codex')
  const independentReviewer = (executor: string) => executor === 'claude' ? 'codex' : 'claude'
  const [reviewer, setReviewer] = useState(task?.reviewAgent || (session.requireReview ? independentReviewer(execution) : ''))
  const [test, setTest] = useState(task?.testId ?? '')
  const [required, setRequired] = useState(task?.requireTest ?? false)
  const [saved, setSaved] = useState(task)
  const [resolution, setResolution] = useState<Resolution>()
  const [busy, setBusy] = useState('')
  const [error, setError] = useState('')
  const project = snapshot.projects.find(item => item.projectId === projectId)
  const node = snapshot.nodes.find(item => item.nodeId === project?.nodeId)
  const capability = node?.adapterCapabilities[execution]
  const ready = node?.availability === 'online' && capability?.execution && (!test || capability.managedTests) &&
    (reviewer ? node.adapterCapabilities[reviewer]?.readOnlyReview : !session.requireReview)
  const complete = title.trim() && goal.trim() && scope.trim() && criteria.trim() && projectId !== ''
  const fields = { projectId, title, goal, scope, acceptanceCriteria: criteria, refName: ref, executionAgent: execution, reviewAgent: reviewer, testId: test, requireTest: required }

  async function run(name: string, operation: () => Promise<void>) {
    setBusy(name); setError('')
    try { await operation() } catch (err) { setError(err instanceof Error ? err.message : '操作失败'); await refresh().catch(() => {}) } finally { setBusy('') }
  }
  async function resolve() {
    await run('base', async () => {
      setResolution(undefined)
      const newResolution = await mutate<Resolution>('projects/' + projectId + '/base-resolutions', { refName: ref }, session)
      for (let retry = 0; retry < 24; retry++) {
        const result = await get<Resolution>('projects/' + projectId + '/base-resolutions/' + newResolution.resolutionId)
        if (result.status === 'resolved') { setResolution(result); return }
        if (result.status === 'failed') throw new Error(result.reason ?? 'Git 基准无法解析')
        await new Promise(resolve => window.setTimeout(resolve, 450))
      }
      throw new Error('节点尚未返回解析结果')
    })
  }
  async function save() {
    const draft = saved
      ? await mutate<Task>('tasks/' + saved.taskId, { ...fields, expectedRevision: saved.revision }, session, 'PATCH')
      : await mutate<Task>('tasks', fields, session)
    setSaved(draft)
    return draft
  }
  function submit(event: FormEvent) {
    event.preventDefault()
    if (!complete || !resolution?.commitId) return
    void run('submit', async () => {
      const draft = saved ?? await save()
      await mutate('tasks/' + draft.taskId + '/submit', { ...fields, expectedRevision: draft.revision, resolutionId: resolution.resolutionId, baseCommit: resolution.commitId }, session)
      await refresh()
      location.hash = 'task/' + draft.taskId
    })
  }
  if (task && task.state !== 'draft') return <Notice>此任务已提交。<a href={'#task/' + task.taskId}> 查看任务</a></Notice>
  return <>
    <a className="back" href={task ? '#task/' + task.taskId : '#'}><ArrowLeft size={16} /> {task ? '返回任务详情' : '返回任务总览'}</a>
    <div className="page-heading"><div><h1>{task ? '编辑草稿' : '新建任务'}</h1><p>给执行 Agent 明确的工作范围和验收条件。</p></div></div>
    <form className="task-form" onSubmit={submit}>
      <section className="panel">
        <div className="panel-heading"><div><h2>任务要求</h2></div></div>
        <div className="form-body">
          <label>任务标题<input value={title} onChange={e => setTitle(e.target.value)} placeholder="例如，修复报告导出失败" maxLength={200} /></label>
          <label>目标<textarea value={goal} onChange={e => setGoal(e.target.value)} placeholder="需要交付什么？" rows={3} maxLength={4000} /></label>
          <label>范围<textarea value={scope} onChange={e => setScope(e.target.value)} placeholder="哪些目录、文件或行为可以改动？" rows={2} maxLength={4000} /></label>
          <label>验收条件<textarea value={criteria} onChange={e => setCriteria(e.target.value)} placeholder="用哪些证据判断任务完成？" rows={3} maxLength={4000} /></label>
        </div>
      </section>
      <section className="panel">
        <div className="panel-heading"><div><h2>工作区与执行</h2></div><Workflow size={20} /></div>
        <fieldset className="form-body" disabled={!!busy}>
          <label>项目仓库<select aria-label="项目仓库" value={projectId} onChange={e => { setProject(e.target.value); setTest(''); setRequired(false); setResolution(undefined) }}>
            <option value="">选择已登记的仓库</option>
            {snapshot.projects.map(item => <option key={item.projectId} value={item.projectId}>{item.name}</option>)}
          </select></label>
          {!snapshot.projects.length && <Notice>请先在安装主机登记仓库。<a href="#settings">查看设置</a></Notice>}
          <label>基准引用<div className="inline-field"><input aria-label="基准引用" value={ref} onChange={e => { setRef(e.target.value); setResolution(undefined) }} placeholder="HEAD、分支或提交" />
            <button type="button" className="button secondary" disabled={!projectId || !ref.trim() || !online || node?.availability !== 'online'} onClick={() => void resolve()}>{busy === 'base' ? '正在解析…' : '解析 Git 基准'}</button></div></label>
          {resolution?.commitId && <div className="commit-confirm"><GitBranch size={16} /> <span>固定提交</span><code>{resolution.commitId}</code></div>}
          <div className="form-columns">
            <label>执行 Agent<select aria-label="执行 Agent" value={execution} onChange={e => { setExecution(e.target.value); setReviewer(session.requireReview ? independentReviewer(e.target.value) : '') }}>
              <option value="codex">Codex · 本地节点</option>
              {(node?.adapterCapabilities.claude?.execution || execution === 'claude') && <option value="claude">Claude Code · 本地节点</option>}
            </select></label>
            <label>复核方式<select aria-label="复核方式" value={reviewer} onChange={e => setReviewer(e.target.value)}>
              {!session.requireReview && <option value="">直接人工验收</option>}
              {execution !== 'claude' && <option value="claude">Claude Code 独立只读复核</option>}
              {execution === 'claude' && <option value="codex">Codex 独立只读复核</option>}
            </select></label>
          </div>
          <label>受管测试<select aria-label="受管测试" value={test} onChange={e => { setTest(e.target.value); if (!e.target.value) setRequired(false) }}>
            <option value="">未指定测试</option>
            {project?.tests?.map(item => <option key={item.testId} value={item.testId}>{item.label}</option>)}
          </select></label>
          {test && <label className="checkbox"><input type="checkbox" checked={required} onChange={e => setRequired(e.target.checked)} /> 验收要求本版本有成功的实际测试记录</label>}
          {project && !ready && <Notice>{node?.availability !== 'online' ? '节点当前离线。' : capability?.reason || (test && !capability?.managedTests ? '受管测试能力尚未就绪。' : '所选执行或复核能力尚未就绪。')} 可以先保存草稿。</Notice>}
        </fieldset>
      </section>
      {error && <Notice danger>{error}</Notice>}
      <div className="form-actions">
        <button type="button" className="button secondary" disabled={!!busy || !online} onClick={() => void run('save', async () => {
          const draft = await save()
          await refresh()
          location.hash = 'task/' + draft.taskId
        })}><Save size={17} /> {busy === 'save' ? '正在保存…' : '保存草稿'}</button>
        <button type="submit" className="button primary" disabled={!!busy || !online || !ready || !complete || !resolution?.commitId}>提交并排队 <ArrowRight size={17} /></button>
      </div>
    </form>
  </>
}
