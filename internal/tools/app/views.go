package app

import (
	"context"
	"slices"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// How a chat shows the calls of one tool, for a tool whose arguments and text are not the best way to read it
type View struct {
	// Tool shown this way, by name
	Tool string
	// Line next to the tool's title in place of its subject argument; empty keeps the subject
	Line func(call Call) string
	// What the result is shown as in place of its text; nil keeps the text
	Result func(call Call, width int, theme theme.Theme) *Shown
	// Only the newest call of the tool in a reply shows its result, the others their line alone: for a tool whose every result holds all
	// it keeps, such as a task list
	Latest bool
	// Settings the view reads, by key, which a chat hands it in Call.Settings as they change
	Settings []string
}

// Call of a tool as a chat has it
type Call struct {
	Arguments string
	// Text of the result, empty while the call runs
	Result string
	Failed bool
	// The reply the call is part of has ended
	Ended bool
	// What the user is shown of the result in place of its text, such as the change a write made; empty when the text is all there is
	Display string
	// Values of the settings the view reads, by key, as YAML
	Settings map[string]string
}

// Result shown as rows under a head that folds them
type Shown struct {
	Head string
	// Head while the rows are folded, Head when empty
	Folded string
	Rows   []string
	// The rows show until the user folds them
	Open bool
	// Picture the first rows draw in text, which a chat whose terminal draws pixel images places over those rows instead; nil for rows of
	// text alone
	Picture *Picture
}

// Picture of a result and the rows it stands over
type Picture struct {
	// Small copy of it as PNG or JPEG bytes, and the file of it at full size, which only a chat on the machine that made it can read
	Data []byte
	Path string
	// Cells it covers: the first Rows of the result's rows, Cols across
	Rows, Cols int
}

type Registrant interface {
	Views() []View
}

// What a chat docks above its prompt for the session it shows, such as the background processes its commands left running: read again
// every second and a half while the session works or something docked counts by the second, every ten seconds otherwise
type Dock struct {
	// Reads what the dock shows of the session, off the UI loop; nil shows nothing
	Load func(ctx context.Context, session string) (Docked, error)
}

// What a dock shows of a session at a moment
type Docked interface {
	// Rows at the width, at most limit of what it lists, one line when folded; none when there is nothing to show
	Rows(width, limit int, now time.Time, t theme.Theme, folded bool) []string
	// Whether something it shows counts by the second, such as a process still running
	Live(now time.Time) bool
}

// Registrant that docks rows in the chats, reading them through the connection. A registrant that docks says so by having this too
type Docker interface {
	Docks(conn *connect.Client) []Dock
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Views() []View {
	var views []View
	for _, registrant := range r.registrants {
		views = append(views, registrant.Views()...)
	}
	return views
}

// Docks of every registrant that docks, reading through the connection
func (r *Registry) Docks(conn *connect.Client) []Dock {
	var docks []Dock
	for _, registrant := range r.registrants {
		if docker, ok := registrant.(Docker); ok {
			docks = append(docks, docker.Docks(conn)...)
		}
	}
	return docks
}

// Keys of every setting the views read
func (r *Registry) Settings() []string {
	var keys []string
	for _, view := range r.Views() {
		for _, key := range view.Settings {
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
	}
	return keys
}

// View of the tool, false when it is shown as its text
func (r *Registry) View(tool string) (View, bool) {
	for _, view := range r.Views() {
		if view.Tool == tool {
			return view, true
		}
	}
	return View{}, false
}

// Views plugins register, read by the chats
type Views struct {
	Registry
}

func New() *Views {
	return &Views{}
}
