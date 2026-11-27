import { Activity, ArrowUpRight, Check, ChevronRight, Circle, CircleAlert, Clock3, Code2, FileText, GitBranch, Layers3, ListChecks, LoaderCircle, MessageCircle, ShieldCheck, X } from 'lucide-react'
import { useEffect, useRef, type ReactNode } from 'react'
import type { Task, TaskState } from './api'

export const statuses: Record<TaskState, string> = {
  draft: '草稿', queued: '排队中', running: '执行中', awaiting_review: '待复核',
  reviewing: '复核中', awaiting_acceptance: '待验收', completed: '已完成', failed: '已失败', cancelled: '已取消',
}
export const blockerLabels: Record<string, string> = { awaiting_approval: '等待授权', awaiting_input: '等待答复', node_disconnected: '节点失联', capability_unavailable: '执行能力不可用' }
export const results: Record<string, string> = {
  allow_once: '允许本次', deny: '已拒绝', answer: '已答复', expired: '已过期', pass: '复核通过',
  request_rework: '要求返工', accept: '已接受', running: '进行中', interrupted: '已中断',
  succeeded: '成功', unknown: '待核对', finished: '已执行', rejected: '未执行', started: '已启动',
  dispatching: '派发中', failed: '失败', cancelled: '已取消',
}
export const eventNames: Record<string, string> = {
  task_changed: '任务状态更新', attempt_changed: '尝试已派发', command_changed: '节点命令更新',
  request_opened: 'Agent 提出请求', request_decided: '所有者处理请求', artifact_published: '产物归档完成',
  review_recorded: '复核结论已记录', acceptance_recorded: '验收已记录', owner_command: '所有者提交操作',
  task_cancelled: '任务已取消', cancel_requested: '已请求取消', historical_report: '已记录历史报告',
}
export const agentName = (agent?: string) => agent === 'codex' ? 'Codex' : agent === 'claude' ? 'Claude Code' : agent || '未选择'
export const date = (time?: string) => time ? new Intl.DateTimeFormat('zh-CN', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false }).format(new Date(time)) : '—'
export const short = (id?: string | null) => id?.slice(0, 9) ?? '—'

export function Brand() {
  return <div className="brand"><div className="brand-mark"><Layers3 size={19} strokeWidth={2.4} /></div><span>NextBot<span className="brand-dot">.</span></span></div>
}
export function Status({ state }: { state: TaskState }) {
  return <span className={'status status-' + state}><span />{statuses[state]}</span>
}
export function Notice({ children, danger = false }: { children: ReactNode; danger?: boolean }) {
  return <div className={'notice' + (danger ? ' danger' : '')} role={danger ? 'alert' : 'status'}><CircleAlert size={16} /><span>{children}</span></div>
}
export function Empty({ icon, title, children }: { icon?: ReactNode; title: string; children?: ReactNode }) {
  return <div className="empty"><div className="empty-icon">{icon ?? <ListChecks size={23} />}</div><strong>{title}</strong>{children && <div>{children}</div>}</div>
}
export function Loading() {
  return <div className="loading" role="status"><LoaderCircle className="spin" size={19} /> 正在加载</div>
}
export function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const dialog = ref.current
    dialog?.showModal()
    return () => dialog?.close()
  }, [])
  return <dialog ref={ref} className="modal" aria-labelledby="modal-title" onCancel={event => { event.preventDefault(); onClose() }}>
    <div className="modal-heading"><h2 id="modal-title">{title}</h2><button type="button" className="icon-button" aria-label="关闭" onClick={onClose}><X size={20} /></button></div>
    {children}
  </dialog>
}
export function TaskGlyph({ task }: { task: Task }) {
  if (task.state === 'completed') return <Check size={18} />
  if (task.state === 'failed' || task.state === 'cancelled') return <CircleAlert size={18} />
  if (task.state === 'draft') return <FileText size={18} />
  if (task.state === 'queued') return <Clock3 size={18} />
  if (task.state === 'running') return <Code2 size={18} />
  return <GitBranch size={18} />
}
export function TaskRow({ task, project, label }: { task: Task; project?: string; label?: string }) {
  const capabilityIssue = task.blockers.find(block => block.kind === 'capability_unavailable')?.message
  return <a className="task-row" href={'#task/' + task.taskId}>
    <div className={'task-glyph ' + task.state}><TaskGlyph task={task} /></div>
    <div className="task-primary"><strong>{task.title || '未命名草稿'}</strong><span>{project || '未选择项目'} <span className="dot-sep">·</span> {label ?? agentName(task.executionAgent)}</span>{capabilityIssue && <span>{capabilityIssue}</span>}</div>
    <div className="task-row-status"><Status state={task.state} /><span>{date(task.updatedAt)}</span></div>
    <ChevronRight size={16} className="row-arrow" />
  </a>
}
export function RequestGlyph({ kind }: { kind: string }) {
  return kind === 'action_approval' ? <ShieldCheck size={18} /> : kind === 'user_question' ? <MessageCircle size={18} /> : <ArrowUpRight size={18} />
}
export const MetricIcons = [ListChecks, Activity, CircleAlert, Check]
export function Dot() { return <Circle size={8} fill="currentColor" /> }
