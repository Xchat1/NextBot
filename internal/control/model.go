package control

import "time"

type Task struct {
	ID          string    `json:"taskId"`
	ProjectID   string    `json:"projectId"`
	Title       string    `json:"title"`
	Goal        string    `json:"goal"`
	Scope       string    `json:"scope"`
	Criteria    string    `json:"acceptanceCriteria"`
	Ref         string    `json:"refName,omitempty"`
	Base        string    `json:"baseCommit"`
	Agent       string    `json:"executionAgent"`
	Reviewer    string    `json:"reviewAgent,omitempty"`
	TestID      string    `json:"testId,omitempty"`
	RequireTest bool      `json:"requireTest"`
	State       string    `json:"state"`
	Blockers    []Blocker `json:"blockers"`
	Revision    string    `json:"revision"`
	AttemptID   *string   `json:"currentAttemptId"`
	ArtifactID  *string   `json:"currentArtifactVersionId"`
	SourceID    string    `json:"sourceTaskId,omitempty"`
	ReworkCount int       `json:"reworkCount"`
	Feedback    string    `json:"feedback,omitempty"`
	CancelID    string    `json:"cancelCommandId,omitempty"`
	CreatedBy   string    `json:"createdBy"`
	UpdatedAt   time.Time `json:"updatedAt"`
	QueuedAt    time.Time `json:"queuedAt,omitempty"`
}
type Blocker struct {
	Kind    string `json:"kind"`
	Source  string `json:"sourceId"`
	Message string `json:"message,omitempty"`
}
type Attempt struct {
	ID         string     `json:"attemptId"`
	TaskID     string     `json:"taskId"`
	Ordinal    int        `json:"ordinal"`
	Role       string     `json:"role"`
	NodeID     string     `json:"nodeId"`
	Agent      string     `json:"agent"`
	Status     string     `json:"status"`
	Workspace  string     `json:"workspaceId,omitempty"`
	StartedAt  *time.Time `json:"startedAt"`
	EndedAt    *time.Time `json:"endedAt"`
	Result     string     `json:"result,omitempty"`
	Recovery   []string   `json:"recoveryOptions"`
	CommandID  string     `json:"commandId"`
	Fence      string     `json:"fence"`
	ArtifactID string     `json:"artifactVersionId,omitempty"`
	ProducerID string     `json:"producerAttemptId,omitempty"`
	SessionID  string     `json:"nativeSessionId,omitempty"`
}
type Entry struct {
	ID        string `json:"entryId"`
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	SHA       string `json:"sha256"`
	Kind      string `json:"evidenceKind"`
}
type Artifact struct {
	ID         string    `json:"artifactVersionId"`
	TaskID     string    `json:"taskId"`
	AttemptID  string    `json:"attemptId"`
	Version    int       `json:"versionNumber"`
	Base       string    `json:"baseCommit"`
	Digest     string    `json:"contentDigest"`
	Entries    []Entry   `json:"manifest"`
	Integrity  string    `json:"integrityStatus"`
	Evidence   bool      `json:"hasRequiredEvidence"`
	Review     *Decision `json:"review,omitempty"`
	Acceptance *Decision `json:"acceptance,omitempty"`
}
type Upload struct {
	ID        string  `json:"uploadId"`
	AttemptID string  `json:"attemptId"`
	Base      string  `json:"baseCommit"`
	Entries   []Entry `json:"manifest"`
	Digest    string  `json:"manifestDigest"`
}
type Request struct {
	ID        string         `json:"requestId"`
	Kind      string         `json:"kind"`
	TaskID    string         `json:"taskId"`
	AttemptID string         `json:"attemptId"`
	Status    string         `json:"status"`
	ExpiresAt time.Time      `json:"expiresAt"`
	Input     map[string]any `json:"frozenInput"`
	Decision  *Decision      `json:"decision"`
	CommandID string         `json:"commandId,omitempty"`
	Result    string         `json:"result,omitempty"`
}
type Decision struct {
	ID        string    `json:"decisionId"`
	Outcome   string    `json:"outcome"`
	At        time.Time `json:"decidedAt"`
	DeviceID  string    `json:"deviceId,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Answer    string    `json:"answerText,omitempty"`
	AttemptID string    `json:"reviewAttemptId,omitempty"`
	Evidence  []string  `json:"evidenceRefs,omitempty"`
}
type Command struct {
	ID        string         `json:"commandId"`
	Type      string         `json:"type"`
	TaskID    string         `json:"taskId,omitempty"`
	AttemptID string         `json:"attemptId,omitempty"`
	Fence     string         `json:"fence,omitempty"`
	LeaseID   string         `json:"leaseId"`
	LeaseAt   time.Time      `json:"leaseExpiresAt"`
	Phase     string         `json:"phase"`
	Payload   map[string]any `json:"payload"`
	Result    string         `json:"result,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}
type Resolution struct {
	ID        string    `json:"resolutionId"`
	ProjectID string    `json:"projectId"`
	NodeID    string    `json:"nodeId"`
	Ref       string    `json:"refName"`
	Commit    string    `json:"commitId,omitempty"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expiresAt"`
	Reason    string    `json:"reason,omitempty"`
}
type TestDefinition struct {
	ID      string   `json:"testId"`
	Label   string   `json:"label"`
	Image   string   `json:"image"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeoutSeconds"`
}
type Project struct {
	ID     string           `json:"projectId"`
	Name   string           `json:"name"`
	NodeID string           `json:"nodeId"`
	Path   string           `json:"path,omitempty"`
	Tests  []TestDefinition `json:"tests"`
}
type Capabilities struct {
	Installed     bool   `json:"installed"`
	Authenticated bool   `json:"authenticated"`
	Version       string `json:"version"`
	Execution     bool   `json:"execution"`
	Review        bool   `json:"readOnlyReview"`
	Tests         bool   `json:"managedTests"`
	Cancel        bool   `json:"cancel"`
	Questions     bool   `json:"questions"`
	Resume        bool   `json:"nativeResume"`
	Reason        string `json:"reason,omitempty"`
}
type Node struct {
	ID           string                  `json:"nodeId"`
	LastSeen     time.Time               `json:"lastSeenAt"`
	Availability string                  `json:"availability"`
	Capabilities map[string]Capabilities `json:"adapterCapabilities"`
	Connection   string                  `json:"connectionId,omitempty"`
	Generation   string                  `json:"generation"`
	BootID       string                  `json:"bootId,omitempty"`
}
type Event struct {
	Stream    string    `json:"streamId"`
	Seq       string    `json:"seq"`
	Type      string    `json:"type"`
	At        time.Time `json:"occurredAt"`
	TaskID    string    `json:"taskId,omitempty"`
	AttemptID string    `json:"attemptId,omitempty"`
	Revision  string    `json:"revision,omitempty"`
	Data      any       `json:"data"`
}
type Device struct {
	ID       string     `json:"deviceId"`
	Name     string     `json:"deviceName"`
	Bound    time.Time  `json:"boundAt"`
	LastSeen time.Time  `json:"lastSeenAt"`
	Revoked  *time.Time `json:"revokedAt"`
	Current  bool       `json:"isCurrent"`
}
type Report struct {
	ID        string         `json:"reportId"`
	TaskID    string         `json:"taskId"`
	AttemptID string         `json:"attemptId"`
	CommandID string         `json:"commandId"`
	Fence     string         `json:"fence"`
	LeaseID   string         `json:"leaseId"`
	Type      string         `json:"type"`
	Phase     string         `json:"phase"`
	Payload   map[string]any `json:"payload"`
	Result    string         `json:"result"`
}
