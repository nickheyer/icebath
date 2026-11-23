package tuigateway

// One scheduled job as the scheduled jobs screen lists it
type CronJobRow struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	// Schedule in the words the store shows it
	Schedule string `json:"schedule"`
	// Schedule as it is typed to make the job again: a cron expression, an every interval or a time
	ScheduleText string `json:"schedule_text"`
	// What the job does now: scheduled, paused, completed, error
	State        string `json:"state"`
	Enabled      bool   `json:"enabled"`
	PausedReason string `json:"paused_reason,omitzero"`
	NextRunAt    string `json:"next_run_at,omitzero"`
	LastRunAt    string `json:"last_run_at,omitzero"`
	LastStatus   string `json:"last_status,omitzero"`
	LastError    string `json:"last_error,omitzero"`
	// A run of the job is going on now
	Running         bool     `json:"running,omitzero"`
	Deliver         string   `json:"deliver,omitzero"`
	Skills          []string `json:"skills,omitzero"`
	Model           string   `json:"model,omitzero"`
	Provider        string   `json:"provider,omitzero"`
	ReasoningEffort string   `json:"reasoning_effort,omitzero"`
	Workdir         string   `json:"workdir,omitzero"`
	Script          string   `json:"script,omitzero"`
	NoAgent         bool     `json:"no_agent,omitzero"`
	Timezone        string   `json:"timezone,omitzero"`
	// Runs the job makes in all, nil for no end, and the runs it made so far
	RepeatTimes *int64 `json:"repeat_times,omitzero"`
	RepeatDone  int64  `json:"repeat_done,omitzero"`
}

type CronJobsListParams struct {
	Profile *string `json:"profile,omitzero"`
	// Paused, completed and failed jobs too
	All bool `json:"all,omitzero"`
}

type CronJobsListResult struct {
	Jobs []CronJobRow `json:"jobs"`
	// Whether the background service starts with the computer, so jobs run while no icebath window is open
	ServiceInstalled bool `json:"service_installed"`
}

// Fields of a job as the job form sends them, a nil field left as it is on an edit
type CronJobFields struct {
	Schedule        *string `json:"schedule,omitzero"`
	Prompt          *string `json:"prompt,omitzero"`
	Name            *string `json:"name,omitzero"`
	Deliver         *string `json:"deliver,omitzero"`
	Model           *string `json:"model,omitzero"`
	Provider        *string `json:"provider,omitzero"`
	ReasoningEffort *string `json:"reasoning_effort,omitzero"`
	Workdir         *string `json:"workdir,omitzero"`
	Repeat          *int64  `json:"repeat,omitzero"`
	// Replaces the job's skills with Skills, an empty list clearing them
	SetSkills bool     `json:"set_skills,omitzero"`
	Skills    []string `json:"skills,omitzero"`
	// Creates the job paused, run only once resumed or run by hand
	Paused bool `json:"paused,omitzero"`
}

// Job to create or change, the job id empty on a create
type CronJobWriteParams struct {
	Profile *string       `json:"profile,omitzero"`
	JobID   string        `json:"job_id,omitzero"`
	Fields  CronJobFields `json:"fields"`
}

// Job one action applies to, with the note a cancel records
type CronJobActionParams struct {
	Profile *string `json:"profile,omitzero"`
	JobID   string  `json:"job_id"`
	Reason  *string `json:"reason,omitzero"`
}

// What a change of a job printed and the job as it stands after it, nil once removed
type CronJobResult struct {
	OK     bool        `json:"ok"`
	Output string      `json:"output"`
	Job    *CronJobRow `json:"job,omitzero"`
}

type CronJobRunsParams struct {
	Profile *string `json:"profile,omitzero"`
	// Job whose runs are read, every job's when empty
	JobID string `json:"job_id,omitzero"`
	Limit int64  `json:"limit,omitzero"`
}

// One durable attempt at running a job
type CronJobRun struct {
	ID         string `json:"id"`
	JobID      string `json:"job_id"`
	Status     string `json:"status"`
	Source     string `json:"source"`
	ClaimedAt  string `json:"claimed_at"`
	StartedAt  string `json:"started_at,omitzero"`
	FinishedAt string `json:"finished_at,omitzero"`
	Error      string `json:"error,omitzero"`
	Delivery   string `json:"delivery,omitzero"`
}

// One saved output of a job's run, named by when it was written
type CronJobOutput struct {
	Name string `json:"name"`
	At   string `json:"at"`
	Size int64  `json:"size"`
}

type CronJobRunsResult struct {
	Runs    []CronJobRun    `json:"runs"`
	Outputs []CronJobOutput `json:"outputs"`
}

type CronJobOutputParams struct {
	Profile *string `json:"profile,omitzero"`
	JobID   string  `json:"job_id"`
	Name    string  `json:"name"`
}

type CronJobOutputResult struct {
	Content string `json:"content"`
}

type CronIncidentsParams struct {
	Profile *string `json:"profile,omitzero"`
	State   *string `json:"state,omitzero"`
}

// One durable failure incident of a job
type CronIncident struct {
	ID          string `json:"id"`
	JobID       string `json:"job_id"`
	State       string `json:"state"`
	FailureType string `json:"failure_type"`
	FirstSeenAt string `json:"first_seen_at"`
	LastSeenAt  string `json:"last_seen_at"`
	Error       string `json:"error"`
	OutputFile  string `json:"output_file,omitzero"`
}

type CronIncidentsResult struct {
	Incidents []CronIncident `json:"incidents"`
}

type CronIncidentAckParams struct {
	Profile *string `json:"profile,omitzero"`
	ID      string  `json:"id"`
}

// Report of the scheduler, doctor or status, or a job's notepad read or changed
type CronReportParams struct {
	Profile *string `json:"profile,omitzero"`
	// doctor, status or notepad
	Command string `json:"command"`
	JobID   string `json:"job_id,omitzero"`
	// Notepad action: list, set or delete, with its key and value
	Action string  `json:"action,omitzero"`
	Key    *string `json:"key,omitzero"`
	Value  *string `json:"value,omitzero"`
}

// What a report printed and whether it ended well
type CronReportResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
}

var (
	// Scheduled jobs of the profile with the state of each
	CronJobsList = NewMethod[CronJobsListParams, CronJobsListResult]("cron.jobs.list")
	// Creates a scheduled job the way icebath cron create does
	CronJobsCreate = NewMethod[CronJobWriteParams, CronJobResult]("cron.jobs.create")
	// Changes a scheduled job the way icebath cron edit does
	CronJobsUpdate = NewMethod[CronJobWriteParams, CronJobResult]("cron.jobs.update")
	// Pauses a scheduled job
	CronJobsPause = NewMethod[CronJobActionParams, CronJobResult]("cron.jobs.pause")
	// Resumes a paused job
	CronJobsResume = NewMethod[CronJobActionParams, CronJobResult]("cron.jobs.resume")
	// Starts a run of a job now, its outcome landing in the job's runs
	CronJobsRun = NewMethod[CronJobActionParams, CronJobResult]("cron.jobs.run")
	// Stops a job's live run, its schedule untouched
	CronJobsCancel = NewMethod[CronJobActionParams, CronJobResult]("cron.jobs.cancel")
	// Removes a scheduled job
	CronJobsRemove = NewMethod[CronJobActionParams, CronJobResult]("cron.jobs.remove")
	// Durable runs of a job and its saved outputs
	CronJobsRuns = NewMethod[CronJobRunsParams, CronJobRunsResult]("cron.jobs.runs")
	// Text of one saved output of a job
	CronJobsOutput = NewMethod[CronJobOutputParams, CronJobOutputResult]("cron.jobs.output")
	// Failure incidents of the scheduled jobs
	CronIncidentsList = NewMethod[CronIncidentsParams, CronIncidentsResult]("cron.incidents.list")
	// Acknowledges and closes one failure incident
	CronIncidentsAck = NewMethod[CronIncidentAckParams, CronReportResult]("cron.incidents.ack")
	// Scheduler doctor or status report, or a job's notepad
	CronReport = NewMethod[CronReportParams, CronReportResult]("cron.report")
)
