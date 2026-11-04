// Window manager cards sharing one environment of theme, keys and settings
package cards

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/ui/gfx"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Rows of scrollback a terminal window keeps when no setting names a count
const DefaultScrollback = 10000

// Live state every card may read, nil members standing for parts the app did not build
type Env struct {
	Theme theme.Theme
	Keys  *keys.Map
	// Lifetime of the app
	Ctx context.Context
	// Setting values as the app holds them keyed by name, empty while unset
	Settings func() map[string]string
	// Directory the active session works in, Root while unset
	WorkDir func() string
	// Directory exports and the shell fallback use
	ProjectDir string
	// Project root the sessions of this instance belong to
	Root string
	// Version of the program, handed to the shells of terminal windows
	Version string
	// Terminal graphics as probed, what the terminal draws and how big its cells are
	Graphics *gfx.Terminal
	// Hands a message to the program from outside its update loop
	Send func(tea.Msg)
}

// First key of the action as a legend shows it
func (e *Env) key(id string) string { return keys.Format(e.Keys.First(id)) }

// Text led by the first key of the action, the text alone while the action is unbound
func (e *Env) keyed(id, text string) string {
	if k := e.key(id); k != "" {
		return k + " " + text
	}
	return text
}

// Image file attached to a message
type Attachment struct {
	Path      string
	MediaType string
	Data      []byte
}

// Input a session composer sends to its agent
type SubmitMsg struct {
	Session     string
	Text        string
	Attachments []Attachment
}

// Slash command a session composer parsed
type CommandMsg struct {
	Session string
	Name    string
	Args    string
}

// Shell line typed with a leading bang, to run in the terminal window beside the session
type RunMsg struct {
	Session string
	Line    string
}

// Setting the app applied, sent to every card so those showing it follow
type SettingChangedMsg struct {
	Key   string
	Value string
}

// Row a list movement action leaves the cursor on, among n rows with page rows in view
func listMove(id string, sel, n, page int) int {
	last := max(n-1, 0)
	switch id {
	case "list.down":
		return min(sel+1, last)
	case "list.up":
		return max(sel-1, 0)
	case "list.top":
		return 0
	case "list.bottom":
		return last
	case "list.page.down":
		return min(sel+max(page, 1), last)
	case "list.page.up":
		return max(sel-max(page, 1), 0)
	}
	return sel
}

// Table lines cut to room rows keeping the header and the cursor row, with the first row index
func window(table []string, sel, room int) ([]string, int) {
	if len(table) <= room || room <= render.TableHead {
		return table, 0
	}
	body := room - render.TableHead
	start := 0
	if sel >= body {
		start = sel - body + 1
	}
	end := min(len(table), render.TableHead+start+body)
	return append(append([]string(nil), table[:render.TableHead]...), table[render.TableHead+start:end]...), start
}

// Dim text centered in w by h, for a card with nothing to show
func emptyView(w, h int, text string, t theme.Theme) string {
	return lipgloss.Place(w, max(h, 1), lipgloss.Center, lipgloss.Center, render.Dim(text, t))
}

// Lines cut or padded to h rows with the foot on the last row, as they are when there is no foot
func footed(lines []string, foot string, h int) string {
	if foot == "" {
		return strings.Join(lines[:min(len(lines), max(h, 0))], "\n")
	}
	body := lines[:min(len(lines), max(h-1, 0))]
	for len(body) < h-1 {
		body = append(body, "")
	}
	return strings.Join(append(body, foot), "\n")
}

// Words of a command line split the way a POSIX shell splits them, honoring quotes and backslashes
func ShellWords(line string) ([]string, error) {
	var words []string
	var cur strings.Builder
	in := false
	quote := rune(0)
	esc := false
	for _, r := range line {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
			in = true
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				esc = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			esc = true
			in = true
		case r == '\'' || r == '"':
			quote = r
			in = true
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, &UnterminatedQuoteError{Quote: quote}
	}
	if esc {
		return nil, &UnterminatedQuoteError{Quote: '\\'}
	}
	if in {
		words = append(words, cur.String())
	}
	return words, nil
}

// Error of ShellWords when a quote or a trailing backslash is left open
type UnterminatedQuoteError struct {
	Quote rune
}

func (e *UnterminatedQuoteError) Error() string {
	if e.Quote == '\\' {
		return "line ends with a backslash"
	}
	return "unterminated " + string(e.Quote) + " quote"
}
