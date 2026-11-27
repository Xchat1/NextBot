# NextBot API 与控制协议

**v0.1 工程设计稿 | 2026-11-27**

本文定义客户端、控制服务与执行节点之间拟采用的协议，以及各端可以据此认定的结果。P1 以同一主机上的单所有者、单 Runner、Codex CLI 为目标。P2 增加 Claude Code 执行适配与独立只读复核。用例与产品状态见[功能规格与验收](../product/03-functional-spec.md)，SQLite 结构、文件落盘顺序、备份与恢复步骤见[数据与恢复设计](03-data-recovery.md)。

## 1. 传输、标识与所有权

| 通道 | 调用方与地址 | 责任 |
| --- | --- | --- |
| 浏览器 REST | 已配对的所有者设备经私网 HTTPS 访问同源 `/api/v1` | 读取快照、历史、产物，提交有条件的命令。浏览器保存设备会话、未确认请求的幂等键和已应用的事件位置。 |
| 浏览器 SSE | 同一设备以会话 Cookie 订阅 `GET /api/v1/events` | 按实例顺序交付已提交事件。事件带资源引用和变化摘要，二进制产物单独读取。 |
| 节点协议 | Runner 经本机专用 Unix domain socket 访问 `/internal/v1` | 主动领取持久命令，上报进程、结果与证据。控制端不向节点另开入站连接。socket 不由 Web 反向代理发布。 |
| 本机管理 | 在安装主机验证操作系统管理权限后使用专用管理入口 | 初始化、丢失所有设备时恢复所有者、登记允许访问的仓库和测试配置。入口与 Runner socket 分权，Agent 子进程无权访问两者。 |

控制服务拥有 SQLite、业务事件和已发布产物。Runner 拥有工作树、CLI 进程和本地执行日志，记录已领取命令、进程身份、已上报结果及待重发报告。控制服务重启后依靠双方记录对账。浏览器、Agent 和 Runner 均不直接写控制服务的数据库。

- JSON 字段为 lowerCamelCase，枚举与事件类型为 snake_case。标识是稳定且不透明的字符串。时间为 UTC RFC 3339。每个项目使用受控的 `projectId`，浏览器提交不含可执行的任意主机路径。
- `taskId` 标识目标，`attemptId` 标识一次执行或复核，`artifactVersionId` 标识一次归档交付。产物的显示号 v1、v2 仅在一个任务内有意义。`requestId` 标识授权或输入问题，`decisionId` 标识已保存的决定。
- `commandId` 是控制端写入队列并交由指定节点处理的命令标识。`reportId` 是 Runner 在本地持久化后上报的记录标识。HTTP 的 `Idempotency-Key` 标识调用方的一次逻辑变更请求，三者作用不同。
- 实例全局事件序号 `seq`、读取水位 `highWater`、任务修订号 `revision` 和客户端条件 `expectedRevision` 在 JSON 与 URL 中均为无符号十进制字符串，零写成 `"0"`，无前导零。比较时按整数大小处理。
- `revision` 属于单个任务。影响主状态、阻塞、当前尝试、有效决定或当前版本的变化，在一次业务事务内使任务修订号增加一次。输出片段及心跳不推进任务修订号。一个业务事务可产生多个连续 `seq`。

### 对象响应

| 对象 | 对外字段 | 约束 |
| --- | --- | --- |
| Task | `taskId, projectId, title, goal, scope, acceptanceCriteria, baseCommit, executionAgent, reviewAgent, state, blockers, revision, currentAttemptId, currentArtifactVersionId, reworkCount, sourceTaskId` | 草稿可缺执行字段。列表和详情返回同一状态定义。尚未产生的当前引用取 null。 |
| Attempt | `attemptId, taskId, ordinal, role, nodeId, workspaceId, agent, status, startedAt, endedAt, result, recoveryOptions` | `role` 为 `execution` 或 `review`。原生会话标识和进程核对信息仅按适用权限返回。 |
| ArtifactVersion | `artifactVersionId, taskId, attemptId, versionNumber, baseCommit, contentDigest, manifest, integrityStatus` | 清单含差异、摘要与测试证据的 `entryId, mediaType, size, sha256, evidenceKind`。证据区分 `observed_run, dry_run, agent_statement, not_run`。 |
| Request / Decision | `requestId, kind, taskId, attemptId, status, expiresAt, frozenInput, decision` | `kind` 为 `action_approval` 或 `user_question`。决定返回 `decisionId, outcome, decidedAt, deviceId`。自动过期的设备为空。 |
| Event | `streamId, seq, type, occurredAt, taskId, attemptId, revision, data` | 实例级事件的任务字段可为空。`data` 只含已定义的变化摘要与资源引用，敏感输入、配对令牌不进入事件流。 |

任务枚举依次为 `draft` 草稿、`queued` 排队中、`running` 执行中、`awaiting_review` 待复核、`reviewing` 复核中、`awaiting_acceptance` 待验收、`completed` 已完成、`failed` 已失败、`cancelled` 已取消。阻塞项分别使用 `awaiting_approval, awaiting_input, node_disconnected, capability_unavailable` 并携带来源对象。排队任务出现执行能力缺失时，`capability_unavailable` 携带具体原因，能力恢复后自动解除。取消请求在节点确认前体现在命令进度中。

## 2. 所有者、配对与会话

首次安装时，控制服务没有所有者设备。管理者通过本机管理入口取得一次性、短时的首次绑定票据。票据由操作者带入预期 HTTPS 来源的绑定页面，浏览器 POST 交换会话。消费票据、绑定设备和关闭初始认领入口必须原子完成。陌生私网设备仅凭访问地址不能获得票据。恢复所有者时由本机管理员重新验证身份，撤销全部设备会话和未用票据，记录审计后签发重绑票据。

下表使用相对 `/api/v1` 路径。票据作为请求体机密提交，配对链接如需携带票据则放在 URL fragment，页面读入后清除。票据不得进入代理访问日志、事件或诊断导出。

| 方法与路径 | 调用身份与输入 | 成功响应 |
| --- | --- | --- |
| POST `/owner/claim` | 本机签发的初始化或恢复票据，`ticket, deviceName`，严格校验来源 | 201，设备描述，设置仅限该设备的会话。已消费或到期的票据返回通用无效结果。 |
| GET `/session` | 已配对设备 | 200，`ownerId, deviceId, instanceId, csrfToken`。恢复后已失效会话返回 401。 |
| POST `/pairings` | 已配对设备，`deviceLabel` 可选 | 201，`pairingId, token, expiresAt`。机密只在授权发起设备的配对响应内返回。 |
| POST `/pairings/claim` | 新设备，`token, deviceName`，严格校验来源 | 201，创建 Device 并设置会话。消费与发证原子完成。同一票据无法领出第二个设备会话。 |
| GET `/devices` | 已配对设备 | 200，设备标识、昵称、绑定与最近访问时间、撤销状态及 `isCurrent`。 |
| POST `/devices/{deviceId}/revoke` | 已配对设备，目标属于同一所有者 | 200，目标撤销时间。服务端检查每次请求、关闭该设备的开放 SSE，并拒绝重连。 |

浏览器会话使用 host-only、Secure、HttpOnly、SameSite=Strict Cookie，不向脚本暴露会话机密。Cookie 路径为 `/`。响应含机密时禁止缓存。所有已登录写请求校验精确 Origin 与会话绑定的 `X-CSRF-Token` 请求头。首次绑定及配对消费没有现存会话，要求预期 Origin、JSON 内容类型和单次票据。跨源读取默认拒绝。SSE、产物读取和分页续读同样核验当前设备授权。

变更请求要求 `Idempotency-Key`。首次绑定及配对消费以一次性票据为额外准入条件，已成功消费后不得通过重试向另一个浏览器重发 Cookie。若响应丢失，先查询 `/session`。无有效会话时从受信设备或本机重新取得票据。签发配对机密使用发起设备作用域，原 token 仅在首次成功响应返回，同一发起设备重试只取得 `pairingId, expiresAt` 和“机密已签发”。丢失该机密时可重新签发，新票据取代该设备未使用的旧票据。普通业务写沿用下一节的所有者级幂等规则。

## 3. 所有者业务 API

下表路径均位于 `/api/v1`，要求已配对的所有者设备。创建以外的任务业务写均携带 `expectedRevision` 和 `Idempotency-Key`。所有列表按稳定次序返回 `items, nextCursor`，游标为不透明值。GET 返回 200，资源不存在或无法读取时返回对应错误。

| 方法与路径 | 输入与前置条件 | 结果 |
| --- | --- | --- |
| GET `/nodes`、GET `/projects` | 当前实例 | 节点连接与最后联系时间、适配器及验证能力、项目和已登记的测试配置摘要。可选基准由节点解析为提交。 |
| POST `/projects/{projectId}/base-resolutions`、GET `/projects/{projectId}/base-resolutions/{resolutionId}` | 写请求传 `refName` 及幂等键，服务端检查项目并交给对应节点解析。客户端用返回的标识查询 | 创建返回 202 与 `resolutionId`。查询返回 200 与 `status, projectId, nodeId, refName, commitId, expiresAt`。成功状态为 `resolved`，保留完整提交标识。 |
| GET `/tasks`、GET `/tasks/{taskId}` | 列表可按状态筛选 | Task 或列表。详情额外返回当前有效待办、可操作项及条件不足的具体原因。 |
| POST `/tasks` | 草稿字段，可含来源任务 `sourceTaskId` | 201，Task，初始状态 `draft`。终态重做有新任务标识并保留来源。 |
| PATCH `/tasks/{taskId}` | 仅草稿，待改字段及修订号 | 200，更新后的 Task。 |
| POST `/tasks/{taskId}/submit` | 完整目标、范围与验收条件，`resolutionId, baseCommit, expectedRevision` 及兼容的 Agent 组合 | 200，验证已保存的解析结果属于该项目、节点且未过期，提交标识匹配后冻结基准并进入 `queued`。不可解析时保留草稿。事务不等待节点。 |
| POST `/tasks/{taskId}/cancel` | 当前非终态，可填 `reason` | 证实命令尚未领取时，在同事务使目标命令不可领取并返回 200。已派发但仍 `queued`、尚未确认启动，以及已运行时，返回 202 与 `commandId, taskId, revision`。Runner 确认未启动或目标进程组已停止后才释放名额。 |
| POST `/tasks/{taskId}/recover` | `attemptId, expectedRevision, choice, evidenceRefs`。选择须属于服务端列出的 `recoveryOptions` | 选择 `reattach` 或 `resume_session` 时返回 202 与持久命令标识，保留原尝试。选择 `new_attempt` 时，控制端须核实原进程退出、已发生的副作用及工作树可安全使用，再重新排队并返回 200、`taskId, state, revision`。本次响应中 `state` 为 `queued`。调度器取得写名额后才建立新尝试并发出 `start_execution`。 |
| GET `/tasks/{taskId}/attempts`、GET `/tasks/{taskId}/artifacts` | 任务标识 | 尝试历史、归档版本及可读状态。当前版本由任务引用给出。 |
| GET `/artifacts/{artifactVersionId}`、GET `/artifacts/{artifactVersionId}/entries/{entryId}` | 精确版本与清单内条目标识，禁止路径寻址 | 返回清单或原始字节及校验标识。缺少已登记文件或校验失败返回 `artifact_unavailable`，已完成任务保留历史验收。 |
| GET `/tasks/{taskId}/timeline` | 时间线分页游标 | 200，任务历史，包括授权、答复、版本和复核引用。时间线按任务筛选，不能用它判定全局事件有缺口。 |
| GET `/requests/{requestId}` | 当前或历史待办标识 | 展示发起者、作用域、到期时间、已生效决定及执行结果摘要。 |
| POST `/requests/{requestId}/decide` | `taskId, attemptId, expectedRevision, inputDigest, outcome`。授权取 `allow_once` 或 `deny`。输入问题取 `answer` 并附 `answerText` | 200，固化生效的 Decision、任务新修订号。仅清除该请求的阻塞。已允许与已实际运行在页面分别呈现。 |
| POST `/tasks/{taskId}/acceptance` | `artifactVersionId, expectedRevision, outcome`。取 `accept` 或 `request_rework`，后者必填 `reason` | 200，决定与 Task。仅接受当前、可校验、满足证据要求的待验收版本。P2 还须匹配当前有效只读复核的通过结论。接受只关闭任务。退回在额度内入队，创建后续尝试与新版交付。 |
| GET `/commands/{commandId}` | 由业务响应取得的命令标识 | 200，所属任务、命令类型、已派发或已确认进度、已知结果。可见对象由所有者权限控制。 |

新建任务和提交是两次显式变更。如果提交回执丢失，客户端应以原键、原请求重试。已失败或已取消任务只能通过新建关联任务重做。P2 默认最多两轮返工，超限时记录等待用户选择的原因，保留当前主状态并返回 `rework_limit` 及最新任务修订号，可选择终止或另建关联任务。

基准解析由 Runner 在 Git 仓库完成。控制服务先持久化 `resolve_base` 命令，再将解析结果写回到 `resolutionId`。用户确认时看到的是已解析的具体提交。原分支后来移动仍使用用户确认的提交。启动前 Runner 再验证该提交仍可读取，找不到时报告具体原因，禁止切换到新的分支尖端。

### 幂等、并发与错误优先级

1. 先验证身份、现行权限、目标归属、Origin 和 CSRF，再解析请求形状。设备已撤销时，历史回执也不可读取。请求错误使用 400 并标为 `invalid_request`。
2. 普通业务写以 `instanceId, ownerId, Idempotency-Key` 为唯一作用域，允许同一所有者的另一台合法设备重发。比较方法、规范化路径与规范化请求体摘要，摘要含修订号，不含会话、设备及 CSRF 头。同键同内容返回原状态码和已提交的结果，可加 `Idempotency-Replayed` 头。响应保留首次生效的设备身份和当时修订号，重放者不能替换 `deviceId`。如需审计本次读取，另记当前调用设备。不同内容占用相同键返回 409 `idempotency_conflict`。并发重发在第一个事务结算后取得同一结果。涉密票据仅按上一节的专用规则返回。
3. 若是新键，先校验该待办或该验收版本是否已经被决定。已决返回 409 `already_decided`，携带生效决定与最新修订号。到期或已关闭的请求拒绝并标明 `request_expired` 或 `request_closed`。迟到输入不得交给当前的新尝试。
4. 再核对客户端修订号、当前尝试和冻结的目标版本。修订号不符返回 409 `revision_conflict`。即使修订号恰好吻合，错误尝试、版本或参数摘要仍返回 409 `stale_target`。读取最新状态后，只有确实希望作一项新的决定时才能使用新键。
5. 最后检查状态转换、只读组合、产物完整性及证据。可区分 409 `invalid_transition, rework_limit, artifact_unavailable, outcome_unknown` 与 422 `capability_unavailable, evidence_missing`。结果未知时禁止自动重放动作。状态变化、决定、幂等回执、事件和必要的持久命令在同一提交中可见。

错误体统一为 `error.code, error.message, error.details, traceId`。可安全公开的 `details` 含最新修订号、冲突对象或已生效决定。401 表示无有效会话，403 表示来源或操作权限校验失败，404 表示找不到可见资源。需要先修正请求或重新作决定的错误不自动重试。503 等临时故障不表示前一次变更一定失败，应用按原键查询结果或重试。不可因幂等回执清理而把旧键当作新操作，至少保留已提交命令的防重索引至该实例历史失效。

## 4. 快照、SSE 与断线续读

每个实例有一条不按任务过滤的客户端事件流。事件在事务提交时取得全局连续序号，从 1 开始，失败事务不留下空号。独立的 `streamId` 表示历史世代。正常重启沿用原值。恢复备份、历史替换等可能倒退水位的操作必须切换世代。客户端持久保存的是 `streamId + seq`。

| 请求 | 协议 |
| --- | --- |
| GET `/api/v1/snapshot` | 返回 `snapshotId, streamId, highWater, tasks, pendingRequests, nodes, projects, nextCursor`。首个响应冻结同一读取视图，所有分页均取该快照的投影。 |
| GET `/api/v1/snapshot?snapshotId=…&cursor=…` | 返回剩余分页，回传同一 `highWater`。快照过期用 410 `snapshot_expired`，丢弃未完成快照并重取，禁止将不同水位的页面拼接。 |
| GET `/api/v1/events?streamId=…&afterSeq=…` | 初次订阅在精确位置之后回放。HTTP 200，`text/event-stream`。SSE 的 id 为 `streamId:seq`，data 为 Event，event 为其 type。心跳使用无 id 的注释。 |

客户端装入完整快照后，自 `highWater` 订阅，逐条应用并保存最后**成功应用**的序号。对同世代旧序号去重。服务端先从持久日志补读，再无缝接实时尾部，订阅建立期间提交的事件也应可读取。观察窗口自然重叠时允许重复，不允许漏掉中间序号。任务界面在客户端筛选，仍推进全局游标。

重连可沿用相同端点并带最后成功应用的 `Last-Event-ID`，两者都在时该头优先于首次 URL 内的起始位置。客户端要能以自己的已应用游标重新建流，不把浏览器已收但尚未落地的事件当成已处理。世代不符、位置早于保留窗口或大于服务端水位时，连接建立前返回 410 `cursor_expired`，要求重新获取完整快照。已建立的连接遇到世代替换时发送无 id 的 `reset_required` 控制通知再关闭。原生 EventSource 无法读取非 200 的错误体时，可改用同源流式 GET 读取响应状态，或在连接反复失败后主动重新验证会话并加载快照。客户端遇到跳号或无法解释的状态变化也执行重新同步，期间禁用陈旧决定按钮。

各端订阅同一组事件。下表仅列 `data` 的必需业务字段，顶层的 `taskId, attemptId, revision, seq` 仍按事件信封返回。

| 事件类型 | data 字段 | 界面动作 |
| --- | --- | --- |
| `task_changed` | `state, blockers, currentAttemptId, currentArtifactVersionId` | 更新任务卡与可用操作。 |
| `attempt_changed`、`command_changed` | 前者含 `status, role`，后者含 `commandId, type, phase` | 标出启动、停止、待核实或已确认进度。 |
| `request_opened`、`request_decided` | 共用 `requestId, kind`。新建带 `expiresAt, inputDigest`，已结算带 `decisionId, outcome, deviceId` | 增删对应待办，明确决定来自哪台设备。 |
| `artifact_published`、`review_recorded` | 共用 `artifactVersionId`。归档带 `versionNumber, integrityStatus`，复核带 `reviewAttemptId, outcome, evidenceRefs` | 关联版本与可查依据，按当前尝试展示。 |
| `node_changed`、`device_revoked` | 节点带 `nodeId, availability, lastSeenAt`，撤权带 `deviceId, revokedAt` | 更新节点和可信设备状态。 |

## 5. Runner 领取、回执和产物交付

Runner 以专用操作系统账号接入 socket。控制端检查 socket 所属权限、对端进程身份与已登记 `nodeId` 的绑定，拒绝浏览器、普通 Agent 进程和其他节点冒报。相同端点的 HTTP 方法沿用上一节约定，下表路径前缀统一为 `/internal/v1`。

| 方法与路径 | Runner 上报 | 控制端响应与效果 |
| --- | --- | --- |
| POST `/nodes/{nodeId}/hello` | `bootId, protocolVersion, adapterCapabilities, activeAttemptIds, oldestPendingReportId` | 200，`connectionId, generation, heartbeatIntervalMs, leaseDurationMs, reconcileAttemptIds`。新连接代际阻止旧连接领取新命令。重连核对未决尝试、待上传日志和写名额。 |
| POST `/nodes/{nodeId}/heartbeat` | `connectionId, generation, activeAttemptIds, activeCommandIds, availability` | 200，服务器时间与需核对对象。可续当前领取租期。失去心跳仅标记失联，绝不据此释放仍可能活动的写名额。 |
| POST `/nodes/{nodeId}/commands/claim` | `connectionId, generation, waitMs, maxCount`，可长轮询 | 200，`items`，无命令时为空。每项含 `commandId, type, taskId, attemptId, fence, leaseId, leaseExpiresAt, payload`。同一待处理命令重新领取仍用原命令标识。 |
| POST `/nodes/{nodeId}/receipts` | `connectionId, generation, fence, leaseId, commandId, reportId, attemptId, phase, result`，`phase` 取 `received, started, finished, rejected, unknown` | 200，原回执或最新记录。命令已领取、已启动、已退出与任务交付分别记录。进度不得倒退。丢失启动结果时传 `unknown` 并进入核对。 |
| POST `/nodes/{nodeId}/reports` | `connectionId, generation, fence, reportId, taskId, attemptId, type, payload`，执行报告关联 `commandId` | 200，`reportId, disposition, seq, revision`。处置结果为 `applied, duplicate, historical`。迟到结果可记入正确的历史尝试，不能改变活动尝试或当前版本。 |
| POST `/nodes/{nodeId}/artifact-uploads` | `attemptId, uploadKey, baseCommit, manifest`，清单含条目标识、内容大小和 SHA-256 | 201，`uploadId, missingEntryIds`。同一上传键和清单可恢复未完成上传。控制端验证尝试归属并限定文件名与总大小。 |
| PUT `/nodes/{nodeId}/artifact-uploads/{uploadId}/entries/{entryId}` | 清单指定条目的字节，匹配声明长度和摘要 | 204。重复相同字节成功。校验不符报 422 `digest_mismatch`，清单外的条目被拒绝。 |
| POST `/nodes/{nodeId}/artifact-uploads/{uploadId}/finalize` | `reportId, attemptId, manifestDigest` | 200，`artifactVersionId, contentDigest, integrityStatus`。只有清单内容完整持久化并核验后发布不可变版本，同一尝试的同一交付重报返回原版本。当前任务是否推进还要验证执行终止与取消意图。 |

`hello` 为注册连接操作，后续 Runner 请求均需有效的连接代际。`generation` 标识本机 Runner 连接，`fence` 标识调度分配的尝试代次，`leaseId` 标识一轮领取。在写尝试中发起受管动作前须复核三者，旧写名额未解除时不分配新写尝试代次。项目级只读查询与 P2 只读复核的 `fence` 为 null。复核按 `attemptId`、指定产物及当前复核任务的所有权条件校验。Runner 变更以稳定业务标识防重，重复内容返回原结果，标识相同而内容不同返回 409。代际过期返回 409 `stale_generation`。旧连接尚未上传的记录保留于本地，当前连接通过对账重送原 `reportId`，控制端校验其原命令、尝试、绑定的旧租约与执行日志证据。历史租约可用于补报该次观察，不能以此启动新的外部动作。

### 节点命令与业务报告

| 命令或报告 | 必带业务内容 | 确认条件 |
| --- | --- | --- |
| 命令 `resolve_base`、报告 `base_resolved` | `resolutionId, projectId, refName`，结果含 `commitId` 或失败原因 | 属于项目级只读查询，`taskId, attemptId, fence` 可为空。只在登记仓库解析，将该结果交回控制端持久保存。 |
| 命令 `start_execution` | 已冻结的任务、项目、基准提交、工作区、Agent、允许能力及执行尝试 | 控制端先保留每节点唯一写名额。Runner 将命令写入本地日志后才确认 `received`。仅据录得的实际启动身份报告 `started`。 |
| 报告 `question_requested`、命令 `deliver_answer` | 提问报告含问题、尝试、到期时间及冻结输入摘要。首个生效答复产生一个定向命令，仅供绑定该尝试的 Runner 经 socket 领取。载荷含 `requestId, decisionId, attemptId, answerText`，附带原生问题引用供适配器定位。 | Runner 校验原尝试、进程和问题仍可接收，按 `decisionId` 在本地登记交付意图，用对应原生输入接口提交一次并记录回执。命令重领或重复上报只补送已知结果。过期、取消或已更换尝试时拒绝交付。 |
| 报告 `approval_requested`、命令 `run_registered_test` | 已登记测试标识、冻结的命令及参数摘要、工作区、待测内容指纹、有效范围和到期时间 | 控制端只对生效允许生成这一项持久命令。Runner 启动前再次验证活动尝试、决定、到期时间及内容指纹。条件变化须申请新授权。被允许的命令只在可强制门控的执行环境运行。 |
| 命令 `cancel_attempt` | 目标尝试、取消发起者、原启动命令引用 | Runner 检查目标进程，终止后上报确认。取消或完成命令竞争以串行化的有效状态为准。已接受取消的尝试即使随后报告自然退出也不送审。 |
| 报告 `execution_finished` | 尝试、停止确认、退出原因、已有受管动作执行记录、归档上传引用 | 退出与归档可任一先到，二者均已核实且无待处理取消，才将成功任务送下一阶段并释放写名额。已确认失败或取消保存已能取得的证据并按终止规则释放。 |
| 报告 `reconciliation_result`、命令 `reattach, resume_session` | 原尝试、可核验进程与日志身份、原生会话能力、工作区与已有副作用证据 | 进程仍在时按验证结果挂接。同一次会话续接沿用尝试号。内容可能已变或执行结果未知时暂停新派发，待所有者从有效恢复选项中选择。 |
| P2 命令 `start_review`、报告 `review_finished` | 新复核尝试、原执行尝试、不可变产物、基准、证据和未决问题。返回 `pass` 或 `request_rework`、理由和已归档条目引用 | 只接受属于当前版本和有效只读复核尝试的结果。证据不够则退回。历史结论仅供追溯。 |

节点重领命令时先查本地日志，已知结果重报原收据。若在记录执行意图后、取得可验证进程身份前崩溃，外部测试可能已启动，标记 `unknown` 并上报。租约到期或收到重复命令均不足以再次启动。只在能够证明未开始、原命令仍有效且上下文一致时才允许首次启动。即使控制端命令已经去重，平台仍须诚实展示无法确认的外部副作用。

所有者及时作出允许或答复决定，而对应命令到期时仍未得到节点接收确认，控制服务会终止原命令，将请求结果记为 `expired`，保留所有者决定，并向仍在等待的原尝试派发一次 `resolve_tool` 到期通知。节点已确认接收的命令沿用其执行和回执流程。

答复同样存在“已送至原生进程、回执未持久化”的窗口。适配器如支持按原生问题标识核实接受结果，可补记已送达。无法证实则报告 `unknown`，供所有者核对，不重复输入。`answerText` 仅在原任务的授权读取和定向交付所需数据中保留。SSE、普通审计日志、错误体和通用诊断只含请求与决定标识及交付状态。

## 6. 业务走读与版本演进

### 一次手机允许的受管测试

以下为虚构的协议数据。桌面和手机读到同一请求 `req_17`。它属于任务 `task_8` 的尝试 `attempt_2`。读取待办时服务端返回 `inputDigest`，绑定预登记测试定义版本、固定参数、工作区、不可变待测内容指纹及允许范围。手机原样带回这一摘要。示例使用固定示意摘要。

手机向 `POST /api/v1/requests/req_17/decide` 发送会话、Origin、CSRF 头和 `Idempotency-Key: allow-req-17-one`，请求体如下。

```json
{
  "taskId": "task_8",
  "attemptId": "attempt_2",
  "expectedRevision": "7",
  "inputDigest": "sha256:04e2aa4be85c18161f05a50d3d127f03adf17f3de28bc826d4eafbc2570ee092",
  "outcome": "allow_once"
}
```

服务端在一个事务中保存决定、更新任务、写入待派命令与事件，返回 200。下面的命令进度表示已排队，节点实际启动和退出另有回执。

```json
{
  "taskId": "task_8",
  "revision": "8",
  "decision": {
    "requestId": "req_17",
    "decisionId": "dec_21",
    "outcome": "allow_once",
    "deviceId": "device_phone",
    "decidedAt": "2026-11-27T08:01:00Z"
  },
  "command": {"commandId": "cmd_38", "type": "run_registered_test", "phase": "queued"}
}
```

桌面从事件流收到下列事件。该次事件后的任务变化或命令进度另占后续连续序号。

```text
id: stream_a:104
event: request_decided
data: {"streamId":"stream_a","seq":"104","type":"request_decided","occurredAt":"2026-11-27T08:01:00Z","taskId":"task_8","attemptId":"attempt_2","revision":"8","data":{"requestId":"req_17","kind":"action_approval","decisionId":"dec_21","outcome":"allow_once","deviceId":"device_phone"}}

```

### 同一任务的阶段差异

| 请求变体 | 请求体的业务字段 | 对应结果或错误 |
| --- | --- | --- |
| 手机答复导出格式问题 | `outcome` 填 `answer`，`answerText` 填“保留原有 CSV 列序”，同时传任务、原尝试、当前修订号和该问题的 `inputDigest`。 | 200 返回生效决定。其他设备以不同键再次答复时返回 409 `already_decided`，携带原决定标识和设备。已换尝试且尚无决定时返回 409 `stale_target` 或该问题已关闭的 `request_closed`。 |
| 从已核实的故障请求新尝试 | `attemptId` 指原尝试，`choice` 填 `new_attempt`，`expectedRevision` 为当前值，`evidenceRefs` 引用进程退出与副作用核实记录。 | 条件满足返回 200 和 `queued`，不返回 Runner 命令标识。原执行结果仍未知时返回 409 `outcome_unknown`，任务保持阻塞。已进入终态时返回 409 `invalid_transition`。 |

**P1 单 Agent。** 电脑创建“修复空数据导出”的草稿并按当前修订号提交。队列为 Codex 发出一个执行命令。运行预登记的空数据回归测试前，手机按冻结的参数和内容指纹批准。桌面若同时拒绝，后到请求收到 `already_decided`，可见生效设备。若测试结束后手机才重新上线，从保存水位补读事件即可获得结果。控制端收到执行退出和完整归档后，任务进入 `awaiting_acceptance`，所有者可接受当前产物。

**P2 双 Agent。** 同一执行协议在成功交付后将任务送入 `awaiting_review`。Claude Code 的只读尝试发现 v1 缺少空数据证据，退回原任务并引用 v1。新执行产物 v2 完成后启动新的复核尝试。已有格式样本和空数据测试均覆盖本次验收条件且得到通过结论时，手机可接受 v2。指向 v1 的迟到结论只进入历史。反向角色组合通过同等能力验证后才开放。

**兼容边界。** P1 使用主版本 1。新增可选字段或 P2 已声明的能力可同版本协商。客户端遇未知字段可忽略，遇未知枚举不得默认当作通过、允许、已完成或可写。Runner 在 hello 公布能力，控制端不向其派发未知命令。改动已有安全语义、必填字段及状态含义需要新协议主版本。P3 跨机 Runner 的网络身份及信任模型单独评审，本稿的同机 socket 凭据不能用作跨机信任依据。
