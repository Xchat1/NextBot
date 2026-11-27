import { ArrowUpRight, ChevronRight, Circle, FolderGit2, Plus } from 'lucide-react'
import { useMemo, useState } from 'react'
import type { Project, Task } from './api'
import { TaskGlyph, statuses } from './ui'

export function WorkspaceTree({ projects, tasks, route, projectId }: { projects: Project[]; tasks: Task[]; route: string; projectId?: string }) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  const [page, selectedTask] = route.split('/')
  const grouped = useMemo(() => {
    const result = new Map<string, Task[]>()
    for (const task of tasks) {
      const group = result.get(task.projectId) ?? []
      group.push(task)
      result.set(task.projectId, group)
    }
    for (const group of result.values()) group.sort((a, b) => b.updatedAt.localeCompare(a.updatedAt))
    return result
  }, [tasks])

  return <nav className="workspace-tree" aria-label="仓库工作区">
    <div className="workspace-tree-heading"><h2>仓库</h2><a className="icon-button" href="#settings" aria-label="管理仓库" title="管理仓库"><ArrowUpRight size={15} /></a></div>
    {!projects.length && <p className="workspace-tree-empty">尚未登记仓库</p>}
    <ul>{projects.map((project, index) => {
      const id = project.projectId
      const active = id === projectId
      const open = expanded[id] ?? (active || index === 0)
      const group = grouped.get(id) ?? []
      const current = active && (page === 'task' || page === 'edit') ? group.find(task => task.taskId === selectedTask) : undefined
      const recent = current && !group.slice(0, 4).includes(current) ? [current, ...group.slice(0, 3)] : group.slice(0, 4)
      return <li key={id}>
        <div className={'workspace-project' + (active ? ' active' : '')}>
          <button type="button" className="tree-toggle" aria-label={(open ? '折叠' : '展开') + project.name} aria-expanded={open}
            aria-controls={'workspace-' + id} onClick={() => setExpanded({ ...expanded, [id]: !open })}><ChevronRight size={14} /></button>
          <a href={'#project/' + id} aria-current={page === 'project' && active ? 'page' : undefined} title={project.name}>
            <FolderGit2 size={16} /><span>{project.name}</span>
          </a>
          <a className="tree-add" href={'#new/' + id} aria-label={'在' + project.name + '中新建任务'} title="在此仓库新建任务"><Plus size={15} /></a>
        </div>
        <ul id={'workspace-' + id} className="workspace-children" hidden={!open}>
          {recent.length === 0 && <li className="workspace-tree-empty">暂无任务</li>}
          {recent.map(task => <li key={task.taskId}>
            <a href={'#task/' + task.taskId} aria-current={current?.taskId === task.taskId ? 'page' : undefined} title={task.title || '未命名草稿'}>
              <span className={'work-state ' + task.state} aria-label={statuses[task.state]}><TaskGlyph task={task} /></span>
              <span>{task.title || '未命名草稿'}</span>
              {task.blockers.length > 0 && <Circle className="work-attention" size={7} fill="currentColor" aria-label="需要关注" />}
            </a>
          </li>)}
          {group.length > recent.length && <li><a className="tree-more" href={'#project/' + id}>查看全部 {group.length} 项</a></li>}
        </ul>
      </li>
    })}</ul>
  </nav>
}
