export type TaskState = 'draft' | 'queued' | 'running' | 'awaiting_review' | 'reviewing' | 'awaiting_acceptance' | 'completed' | 'failed' | 'cancelled'
export type Task = {
  taskId: string
  projectId: string
  title: string
  goal: string
  scope: string
  acceptanceCriteria: string
  refName?: string
  baseCommit: string
  executionAgent: string
  reviewAgent?: string
  testId?: string
  requireTest: boolean
  state: TaskState
  blockers: { kind: string; sourceId: string; message?: string }[]
  revision: string
  currentAttemptId: string | null
  currentArtifactVersionId: string | null
  sourceTaskId?: string
  reworkCount: number
  feedback?: string
  cancelCommandId?: string
  updatedAt: string
  requests?: OwnerRequest[]
}
export type OwnerRequest = {
  requestId: string
  kind: 'action_approval' | 'user_question'
  taskId: string
  attemptId: string
  status: string
  expiresAt: string
  frozenInput: { inputDigest: string; question?: string; label?: string; args?: string[]; image?: string; scope?: string; snapshotHash?: string; workspaceId?: string; timeoutSeconds?: number }
  decision?: Decision
  result?: string
}
export type Decision = { decisionId: string; outcome: string; deviceId?: string; decidedAt: string; reason?: string; answerText?: string; evidenceRefs?: string[] }
export type Attempt = { attemptId: string; ordinal: number; role: string; agent: string; status: string; startedAt?: string; endedAt?: string; result?: string; recoveryOptions: string[]; workspaceId?: string; nativeSessionId?: string }
export type Artifact = { artifactVersionId: string; versionNumber: number; baseCommit: string; contentDigest: string; hasRequiredEvidence: boolean; integrityStatus: string; manifest: Entry[]; review?: Decision; acceptance?: Decision }
export type Entry = { entryId: string; evidenceKind: string; mediaType: string; sha256: string; size: number }
export type TimelineEvent = { streamId: string; seq: string; type: string; occurredAt: string; taskId?: string; attemptId?: string; data: Record<string, unknown> | null }
export type Project = { projectId: string; name: string; nodeId: string; tests: { testId: string; label: string; args: string[] }[] }
export type Capability = { installed: boolean; authenticated: boolean; version: string; execution: boolean; readOnlyReview: boolean; managedTests: boolean; questions: boolean; cancel: boolean; nativeResume: boolean; reason?: string }
export type Node = { nodeId: string; lastSeenAt: string; availability: string; adapterCapabilities: Record<string, Capability> }
export type Device = { deviceId: string; deviceName: string; boundAt: string; lastSeenAt: string; revokedAt?: string; isCurrent: boolean }
export type Snapshot = { snapshotId: string; streamId: string; highWater: string; tasks: Task[]; pendingRequests: OwnerRequest[]; nodes: Node[]; projects: Project[]; nextCursor?: string }
export type Session = { ownerId: string; deviceId: string; instanceId: string; csrfToken: string }
export type List<T> = { items: T[]; nextCursor?: string }
export type Resolution = { resolutionId: string; status: string; commitId?: string; reason?: string }

export class APIError extends Error {
  constructor(public status: number, public code: string, message: string, public details?: { decision?: Decision }) {
    super(message)
  }
}
async function check(response: Response) {
  if (response.ok) return response
  const body = await response.json().catch(() => null)
  throw new APIError(response.status, body?.error?.code ?? 'network_error', body?.error?.message ?? '无法完成请求', body?.error?.details)
}
export function apiPath(path: string) { return '/api/v1/' + path }
export async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  return (await check(await fetch(apiPath(path), { signal }))).json()
}
export async function getText(path: string, signal?: AbortSignal) {
  return (await check(await fetch(apiPath(path), { signal }))).text()
}
type Pending = { key: string; path: string; method: string; body: Record<string, unknown> }
const pendingKey = (session: Session) => 'nextbot:pending:' + session.deviceId
function pending(session: Session): Pending[] {
  try { return JSON.parse(sessionStorage.getItem(pendingKey(session)) ?? '[]') } catch { return [] }
}
function save(session: Session, entries: Pending[]) {
  try { sessionStorage.setItem(pendingKey(session), JSON.stringify(entries)) } catch { /* Storage can be disabled. */ }
}
async function send<T>(command: Pending, session?: Session): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json', 'Idempotency-Key': command.key }
  if (session) headers['X-CSRF-Token'] = session.csrfToken
  const response = await fetch(apiPath(command.path), { method: command.method, headers, body: JSON.stringify(command.body) })
  if (session && response.status < 500) save(session, pending(session).filter(entry => entry.key !== command.key))
  return (await check(response)).json()
}
function key() { return globalThis.crypto.randomUUID?.() ?? Array.from(globalThis.crypto.getRandomValues(new Uint32Array(4)), v => v.toString(16).padStart(8, '0')).join('') }
export async function mutate<T>(path: string, body: Record<string, unknown>, session?: Session, method = 'POST'): Promise<T> {
  const prior = session ? pending(session) : []
  const previous = prior.find(entry => entry.path === path && entry.method === method && JSON.stringify(entry.body) === JSON.stringify(body))
  const command = previous ?? { path, body, method, key: key() }
  if (session && !previous) save(session, [...prior, command])
  return send<T>(command, session)
}
export async function replay(session: Session) {
  // Every uncertain request keeps its original route, body and idempotency key.
  for (const entry of pending(session)) {
    try { await send(entry, session) } catch (error) { if (!(error instanceof APIError) || error.status >= 500) throw error }
  }
}
export async function snapshot(signal?: AbortSignal) {
  const out = await get<Snapshot>('snapshot', signal)
  let cursor = out.nextCursor
  while (cursor) {
    const page = await get<Snapshot>('snapshot?' + new URLSearchParams({ snapshotId: out.snapshotId, cursor }), signal)
    if (page.highWater !== out.highWater || page.streamId !== out.streamId) throw new Error('快照读取位置已变更')
    out.tasks.push(...page.tasks)
    cursor = page.nextCursor
  }
  return out
}
export async function followEvents(stream: string, after: string, signal: AbortSignal, onReady: () => void, onEvent: (event: TimelineEvent) => void) {
  const url = apiPath('events?' + new URLSearchParams({ streamId: stream, afterSeq: after }))
  const response = await check(await fetch(url, { signal, headers: { 'Last-Event-ID': stream + ':' + after } }))
  if (!response.body) throw new Error('事件流不可用')
  onReady()
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader()
  let buffer = ''
  let position = BigInt(after)
  try {
    for (;;) {
      const { value, done } = await reader.read()
      if (done) break
      buffer += value
      let boundary = buffer.indexOf('\n\n')
      while (boundary >= 0) {
        const frame = buffer.slice(0, boundary)
        buffer = buffer.slice(boundary + 2)
        const data = frame.split('\n').find(line => line.startsWith('data: '))?.slice(6)
        if (frame.includes('event: reset_required')) throw new APIError(410, 'cursor_expired', '事件位置已失效')
        if (data) {
          const event: TimelineEvent = JSON.parse(data)
          const seq = BigInt(event.seq)
          if (event.streamId !== stream || seq > position + 1n) throw new APIError(410, 'cursor_expired', '事件位置已失效')
          if (seq > position) { position = seq; onEvent(event) }
        }
        boundary = buffer.indexOf('\n\n')
      }
    }
  } finally {
    await reader.cancel().catch(() => {})
  }
}
