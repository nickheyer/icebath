package tools

// Action a tool asks the user to allow, which the approvals decide on
type Gate struct {
	// Name the answer is remembered under, for the session or always in the allowlist; the description when empty
	Key string
	// What it is flagged as, as the user reads it; the key when empty
	Description string
	// What would run or change, as shown to the user
	Target string
	// Asked every time whatever the mode, yolo, the allowlist or an earlier answer say, with no answer that lasts past this one: for
	// actions the user must see each time, such as edits to the files that steer agents
	Strict bool
	// What the model reads when a scheduled job, a one-shot run or an unattended surface refuses it without asking; a general text when
	// empty
	Blocked string
	// What the refusal of a one-shot run or a scheduled job says needed approval, such as "computer_use `click` requires approval", the
	// kind of action it allows and what to do instead; the description, flagged actions and a general advice when empty
	Subject, Noun, Advice string
	// What the model reads when nobody is there to ask, a general text when empty
	Unanswered string
	// The action runs code the user is shown, such as a Python script that can reach past the command checks: let through on the user's
	// own window and asked of them only on a surface they reach from elsewhere, such as a chat app, or when ICEBATH_EXEC_ASK says to ask,
	// the smart reviewer first; Subject says what the code may do in the refusal of a context nobody watches
	Code bool
}

// Why a flagged action or command did not go ahead
type Refusal struct {
	// What the model reads
	Message string
	// One line the user reads of what became of it, empty when there is nothing to add
	Summary string
	// How it ended: denied, timeout, cancelled, blocked, or how a rule refused it
	Outcome string
	// What the user said when they denied it
	Reason string
}

func (r *Refusal) Error() string { return r.Message }

// What the approvals made of a command about to run
type Verdict struct {
	// Why it may not run, nil when it may
	Refusal *Refusal
	// How it was let through after it was flagged, which the result tells the model; empty when nothing flagged it
	Note string
	// The user allowed it, so a message they sent while it waited is no reason to put it aside
	User bool
}
