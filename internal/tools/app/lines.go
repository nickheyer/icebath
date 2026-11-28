package app

import (
	"context"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// What runs a line a person types in a chat after a mark of its own: what it prints is shown in the chat as it comes, and kept with the
// session
type Liner struct {
	// Mark the line starts with
	Mark string
	// What the empty prompt says of the mark, such as that it runs a line in the person's shell; empty says nothing
	Hint func(env *cards.Env) string
	// Starts the line for a chat, telling each change of what it printed and how it ended to told, off the UI loop, until it ends; stop
	// ends it
	Run func(ctx context.Context, run Run, told func(ran Ran)) (stop func())
	// Lines run in a session before, oldest first
	Kept func(ctx context.Context, session, profile string) ([]Ran, error)
	// What a chat shows of a line, as rows under a head that folds them, naming the key that stops a line still running
	Shown func(ran Ran, width int, t theme.Theme, stop string) *Shown
}

// A line to run, and the chat it is run for
type Run struct {
	// Session of the chat, empty before it has one, and its profile
	Session, Profile string
	// Folder of a chat that has no session yet
	Folder string
	Line   string
	// The person's shell, as this app knows it
	Shell string
}

// A line run, or running, and what it printed so far
type Ran struct {
	Time   time.Time
	Line   string
	Output string
	// Only the end of the output was kept
	Cut  bool
	Done bool
	Code int
	// The person stopped it
	Stopped bool
	// Why it could not run, empty when it ran
	Error string
	// Folder it ran in, once the server said it
	Folder string
}

// Registrant that runs lines a person types in a chat. A registrant that does says so by having this too
type Lines interface {
	Lines(conn *connect.Client) []Liner
}

// Liners of every registrant that runs lines, reaching what runs them through the connection
func (r *Registry) Lines(conn *connect.Client) []Liner {
	var out []Liner
	for _, registrant := range r.registrants {
		if lines, ok := registrant.(Lines); ok {
			out = append(out, lines.Lines(conn)...)
		}
	}
	return out
}
