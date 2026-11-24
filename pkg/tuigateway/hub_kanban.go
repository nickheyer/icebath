package tuigateway

// One kanban board with its task counts by column
type KanbanBoard struct {
	Slug        string           `json:"slug"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitzero"`
	Icon        string           `json:"icon,omitzero"`
	Current     bool             `json:"current,omitzero"`
	Archived    bool             `json:"archived,omitzero"`
	Counts      map[string]int64 `json:"counts,omitzero"`
}

// One column of a board in board order
type KanbanColumn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Icon  string `json:"icon"`
}

// One task as the board shows it
type KanbanTaskRow struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Body        string   `json:"body,omitzero"`
	Status      string   `json:"status"`
	Assignee    string   `json:"assignee,omitzero"`
	Priority    int64    `json:"priority,omitzero"`
	CreatedBy   string   `json:"created_by,omitzero"`
	CreatedAt   int64    `json:"created_at,omitzero"`
	StartedAt   int64    `json:"started_at,omitzero"`
	CompletedAt int64    `json:"completed_at,omitzero"`
	Skills      []string `json:"skills,omitzero"`
	Result      string   `json:"result,omitzero"`
	LastError   string   `json:"last_error,omitzero"`
	Model       string   `json:"model,omitzero"`
	BlockKind   string   `json:"block_kind,omitzero"`
}

type KanbanBoardParams struct {
	Profile *string `json:"profile,omitzero"`
	// Board read, the current board when empty
	Board string `json:"board,omitzero"`
	// Archived tasks too
	Archived bool `json:"archived,omitzero"`
}

// One board in full: every board for switching, the columns in order and the tasks
type KanbanBoardResult struct {
	Board   string          `json:"board"`
	Boards  []KanbanBoard   `json:"boards"`
	Columns []KanbanColumn  `json:"columns"`
	Tasks   []KanbanTaskRow `json:"tasks"`
	// Profiles tasks can be assigned to
	Assignees []string `json:"assignees,omitzero"`
}

type KanbanTaskParams struct {
	Profile *string `json:"profile,omitzero"`
	Board   string  `json:"board,omitzero"`
	TaskID  string  `json:"task_id"`
}

// One comment of a task
type KanbanComment struct {
	ID        int64  `json:"id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"`
}

// One attempt a worker made at a task
type KanbanAttempt struct {
	ID        int64  `json:"id"`
	Profile   string `json:"profile,omitzero"`
	Status    string `json:"status"`
	Outcome   string `json:"outcome,omitzero"`
	Summary   string `json:"summary,omitzero"`
	Error     string `json:"error,omitzero"`
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at,omitzero"`
}

// A task with its comments, attempts and links
type KanbanTaskResult struct {
	Task     KanbanTaskRow   `json:"task"`
	Comments []KanbanComment `json:"comments"`
	Runs     []KanbanAttempt `json:"runs"`
	Parents  []string        `json:"parents,omitzero"`
	Children []string        `json:"children,omitzero"`
}

// Kanban command run the way icebath kanban runs it, its words after kanban, on a board
type KanbanCommandParams struct {
	Profile *string  `json:"profile,omitzero"`
	Board   string   `json:"board,omitzero"`
	Args    []string `json:"args"`
}

// What a kanban command printed and whether it ended well
type KanbanCommandResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
}

// Events of a board after a cursor, for a page that follows them: one task's or every task's, narrowed by assignee, tenant and kind
type KanbanEventsParams struct {
	Profile *string `json:"profile,omitzero"`
	Board   string  `json:"board,omitzero"`
	TaskID  string  `json:"task_id,omitzero"`
	// Events after this id; zero reads the newest ones
	After    int64    `json:"after,omitzero"`
	Assignee string   `json:"assignee,omitzero"`
	Tenant   string   `json:"tenant,omitzero"`
	Kinds    []string `json:"kinds,omitzero"`
	// Most events answered, 200 when absent
	Limit int64 `json:"limit,omitzero"`
}

// One event of a task as the events page shows it
type KanbanEvent struct {
	ID        int64  `json:"id"`
	TaskID    string `json:"task_id"`
	Title     string `json:"title,omitzero"`
	Kind      string `json:"kind"`
	Payload   string `json:"payload,omitzero"`
	CreatedAt int64  `json:"created_at"`
	Assignee  string `json:"assignee,omitzero"`
}

// Events in order, oldest first, and the cursor the next read starts after
type KanbanEventsResult struct {
	Events []KanbanEvent `json:"events"`
	Cursor int64         `json:"cursor"`
}

var (
	// One board with its columns and tasks, and every board to switch to
	KanbanBoardView = NewMethod[KanbanBoardParams, KanbanBoardResult]("kanban.board.view")
	// Board verbs: create, switch, rename, archive
	KanbanBoardCommand = NewMethod[KanbanCommandParams, KanbanCommandResult]("kanban.board.command")
	// One task with its comments and attempts
	KanbanTaskShow = NewMethod[KanbanTaskParams, KanbanTaskResult]("kanban.task.show")
	// Task verbs: create, edit, assign, comment, block, unblock, promote, complete, reopen, archive and the rest that return at once
	KanbanTaskCommand = NewMethod[KanbanCommandParams, KanbanCommandResult]("kanban.task.command")
	// Events of a board or one task after a cursor, for the page that follows them
	KanbanEvents = NewMethod[KanbanEventsParams, KanbanEventsResult]("kanban.events")
)
