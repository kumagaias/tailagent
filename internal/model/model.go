package model

import "time"

type Agent struct {
	ID           int64         `json:"id"`
	Type         string        `json:"type"`
	CommandPath  string        `json:"command_path"`
	Instruction  string        `json:"instruction"`
	Status       string        `json:"status"`
	Capabilities []string      `json:"capabilities"`
	Account      *AgentAccount `json:"account,omitempty"`
	Usage        *AgentUsage   `json:"usage,omitempty"`
	LastRun      *time.Time    `json:"last_run,omitempty"`
	ActiveTask   string        `json:"active_task,omitempty"`
	LastError    string        `json:"last_error,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

type AgentAccount struct {
	AuthMode string `json:"auth_mode,omitempty"`
	Login    string `json:"login,omitempty"`
	Plan     string `json:"plan,omitempty"`
}

type AgentUsage struct {
	LifetimeTokens  *int64     `json:"lifetime_tokens,omitempty"`
	RemainingPct    *int       `json:"remaining_percent,omitempty"`
	ResetsAt        *time.Time `json:"resets_at,omitempty"`
	LimitName       string     `json:"limit_name,omitempty"`
	UnavailableText string     `json:"unavailable_text,omitempty"`
}

type Project struct {
	ID                 int64      `json:"id"`
	Name               string     `json:"name"`
	Prefix             string     `json:"prefix,omitempty"`
	FolderPath         string     `json:"folder_path"`
	GitRoot            string     `json:"git_root,omitempty"`
	GitRemote          string     `json:"git_remote,omitempty"`
	DefaultMilestoneID int64      `json:"default_milestone_id"`
	MilestoneCount     int        `json:"milestone_count"`
	TaskCount          int        `json:"task_count"`
	LastRun            *time.Time `json:"last_run,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type Milestone struct {
	ID            int64     `json:"id"`
	ProjectID     int64     `json:"project_id"`
	ProjectName   string    `json:"project_name,omitempty"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	Status        string    `json:"status"`
	TargetDate    *string   `json:"target_date,omitempty"`
	IsDefaultNone bool      `json:"is_default_none"`
	TaskCount     int       `json:"task_count"`
	DoneCount     int       `json:"done_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Task struct {
	ID                 int64           `json:"id"`
	DisplayID          string          `json:"display_id"`
	ProjectNumber      *int64          `json:"project_number,omitempty"`
	ProjectID          int64           `json:"project_id"`
	ProjectName        string          `json:"project_name,omitempty"`
	MilestoneID        int64           `json:"milestone_id"`
	MilestoneName      string          `json:"milestone_name,omitempty"`
	Title              string          `json:"title"`
	Description        string          `json:"description"`
	Label              string          `json:"label"`
	LabelAuto          bool            `json:"label_auto"`
	Status             string          `json:"status"`
	AssignedAgentID    *int64          `json:"assigned_agent_id,omitempty"`
	AssignedAgentType  string          `json:"assigned_agent_type,omitempty"`
	Instruction        string          `json:"instruction"`
	AcceptanceCriteria string          `json:"acceptance_criteria"`
	ImageName          string          `json:"image_name,omitempty"`
	ImageContentType   string          `json:"image_content_type,omitempty"`
	ImageSize          int64           `json:"image_size,omitempty"`
	Images             []TaskImageMeta `json:"images,omitempty"`
	LatestRun          *Run            `json:"latest_run,omitempty"`
	LatestLog          string          `json:"latest_log,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

type TaskImage struct {
	ID          int64
	TaskID      int64
	Name        string
	ContentType string
	Size        int64
	Data        []byte
}

type TaskImageMeta struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

type Run struct {
	ID               int64      `json:"id"`
	ProjectID        int64      `json:"project_id"`
	ProjectName      string     `json:"project_name,omitempty"`
	MilestoneID      int64      `json:"milestone_id"`
	TaskID           int64      `json:"task_id"`
	TaskTitle        string     `json:"task_title,omitempty"`
	AgentID          int64      `json:"agent_id"`
	AgentType        string     `json:"agent_type,omitempty"`
	Status           string     `json:"status"`
	Instruction      string     `json:"instruction"`
	WorkingDirectory string     `json:"working_directory"`
	ProcessID        *int       `json:"process_id,omitempty"`
	ExitCode         *int       `json:"exit_code,omitempty"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	TraceID          string     `json:"trace_id"`
	Error            string     `json:"error,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

type Approval struct {
	ID          int64      `json:"id"`
	RunID       int64      `json:"run_id"`
	AgentID     int64      `json:"agent_id"`
	AgentType   string     `json:"agent_type,omitempty"`
	ProjectID   int64      `json:"project_id"`
	ProjectName string     `json:"project_name,omitempty"`
	TaskID      int64      `json:"task_id"`
	TaskTitle   string     `json:"task_title,omitempty"`
	RequestType string     `json:"request_type"`
	Operation   string     `json:"operation"`
	Reason      string     `json:"reason"`
	Risk        string     `json:"risk"`
	Status      string     `json:"status"`
	Decision    string     `json:"decision,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	ExpiresAt   time.Time  `json:"expires_at"`
}

type TraceEvent struct {
	ID            int64          `json:"id"`
	TraceID       string         `json:"trace_id"`
	RunID         int64          `json:"run_id"`
	ParentEventID *int64         `json:"parent_event_id,omitempty"`
	EventType     string         `json:"event_type"`
	Status        string         `json:"status"`
	Message       string         `json:"message"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	StartedAt     time.Time      `json:"started_at"`
	EndedAt       *time.Time     `json:"ended_at,omitempty"`
	DurationMS    *int64         `json:"duration_ms,omitempty"`
	AgentType     string         `json:"agent_type,omitempty"`
	ProjectName   string         `json:"project_name,omitempty"`
	TaskTitle     string         `json:"task_title,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

type RunLog struct {
	ID        int64     `json:"id"`
	RunID     int64     `json:"run_id"`
	Stream    string    `json:"stream"`
	Sequence  int64     `json:"sequence"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type Chat struct {
	ID          int64     `json:"id"`
	ProjectID   int64     `json:"project_id"`
	ProjectName string    `json:"project_name,omitempty"`
	TaskID      int64     `json:"task_id"`
	TaskTitle   string    `json:"task_title,omitempty"`
	AgentID     int64     `json:"agent_id"`
	AgentType   string    `json:"agent_type,omitempty"`
	Title       string    `json:"title"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ChatMessage struct {
	ID           int64             `json:"id"`
	ChatID       int64             `json:"chat_id"`
	RunID        *int64            `json:"run_id,omitempty"`
	Role         string            `json:"role"`
	Content      string            `json:"content"`
	Run          *Run              `json:"run,omitempty"`
	Logs         []RunLog          `json:"logs,omitempty"`
	TaskProposal *ChatTaskProposal `json:"task_proposal,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
}

type ChatTaskProposal struct {
	MessageID      int64              `json:"message_id"`
	Status         string             `json:"status"`
	Tasks          []TaskProposalItem `json:"tasks"`
	CreatedTaskIDs []int64            `json:"created_task_ids,omitempty"`
}

type TaskProposalItem struct {
	Title              string `json:"title"`
	Description        string `json:"description,omitempty"`
	Label              string `json:"label,omitempty"`
	LabelAuto          bool   `json:"label_auto,omitempty"`
	MilestoneID        int64  `json:"milestone_id,omitempty"`
	Instruction        string `json:"instruction,omitempty"`
	AcceptanceCriteria string `json:"acceptance_criteria,omitempty"`
}

type Settings struct {
	WorkspaceRoot       string `json:"workspace_root"`
	DatabasePath        string `json:"database_path"`
	ApprovalTimeoutSecs int    `json:"approval_timeout_seconds"`
	DefaultShell        string `json:"default_shell"`
	MaxConcurrentAgents int    `json:"max_concurrent_agents"`
	TraceRetentionDays  int    `json:"trace_retention_days"`
	OTelEndpoint        string `json:"otel_endpoint"`
}

type Dashboard struct {
	Agents           int `json:"agents"`
	Projects         int `json:"projects"`
	PendingApprovals int `json:"pending_approvals"`
	Running          int `json:"running"`
}
