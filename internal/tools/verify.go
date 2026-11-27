package tools

import "context"

// One command a session ran in the foreground, as the terminal hands it to the registrants that keep verification evidence
type Run struct {
	Command string
	// Folder it ran in
	Cwd     string
	Session string
	Code    int
	// Output as the model reads it, secrets masked
	Output string
	// Exit codes of the commands of its last pipeline in their order, when the shell reported them
	Pipeline []int
}

// What a check a session ran proved, as the result of its command tells the model
type Evidence struct {
	// passed or failed
	Status string `json:"status"`
	// What kind of check it was, such as test, lint, build or verify
	Kind string `json:"kind"`
	// full, or targeted for a check of some files or tests alone
	Scope string `json:"scope"`
	// The project's own command it ran
	Canonical string `json:"canonical_command"`
}

// Registrant that keeps evidence that the code a session changes was checked: told of each command it runs in the foreground, and asked
// before a turn that changed files ends whether the agent must check them first
type Verifier interface {
	// Records the run when it checks the session's workspace, giving what it proved; nil for a run that checks nothing
	Verified(env *Env, run Run) *Evidence
	// Message that sends the agent back to check the files the turn changed before it finishes, empty when nothing is due
	Unverified(env *Env, session string, changed []string) string
	// Most times the registrants that keep a turn going may send the agent back in one turn
	Nudges(env *Env) int
}

// What the first registrant that keeps verification evidence made of the run, nil when none recorded it
func (r *Registry) Verified(env *Env, run Run) *Evidence {
	for _, registrant := range r.registrants {
		if verifier, ok := registrant.(Verifier); ok {
			if evidence := verifier.Verified(env, run); evidence != nil {
				return evidence
			}
		}
	}
	return nil
}

// Message of the first registrant that keeps verification evidence sending the agent back to check the files the turn changed, empty when
// none has one
func (r *Registry) Unverified(env *Env, session string, changed []string) string {
	for _, registrant := range r.registrants {
		if verifier, ok := registrant.(Verifier); ok {
			if message := verifier.Unverified(env, session, changed); message != "" {
				return message
			}
		}
	}
	return ""
}

// What a turn that changed code is about to end with, for the registrants that may keep it going
type Ending struct {
	Session  string
	Platform string
	Model    string
	// The session works on code, in a project the coding posture applies to
	Coding bool
	// Times the registrants kept this turn going so far
	Attempt int
	// The answer the turn is about to end with
	Final string
	// Files the turn's writes landed in, sorted
	Changed []string
}

// Registrant that may send the agent back once more before a turn that changed code ends, after the verification evidence said nothing
// was due, such as the pre_verify hooks: the message the agent is sent back with, empty to let the turn end
type Continuer interface {
	Continue(ctx context.Context, env *Env, ending Ending) string
}

// Message of the first registrant that keeps the turn going, empty when none does or the turn was kept going as often as the first registrant
// that keeps verification evidence allows
func (r *Registry) Continue(ctx context.Context, env *Env, ending Ending) string {
	for _, registrant := range r.registrants {
		if verifier, ok := registrant.(Verifier); ok {
			if ending.Attempt >= verifier.Nudges(env) {
				return ""
			}
			break
		}
	}
	for _, registrant := range r.registrants {
		if continuer, ok := registrant.(Continuer); ok {
			if message := continuer.Continue(ctx, env, ending); message != "" {
				return message
			}
		}
	}
	return ""
}
