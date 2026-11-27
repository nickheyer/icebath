package app

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	"github.com/nickheyer/icebath/pkg/ui/keys"
)

// How often the docks of a chat are read while its session works or something docked counts by the second, and while all is idle
const (
	dockEvery     = 1500 * time.Millisecond
	dockIdleEvery = 10 * time.Second
	// Rows the docks list at most, each a row the transcript loses
	dockMaxRows = 5
)

var dockKeys = []keys.Action{
	{ID: "agent.dock", Context: keys.Session, Label: "dock", Doc: "fold the dock of background processes above the prompt to a line, or unfold it", Default: []string{"f7"}},
}

// Reads the docks of the session while the chat watches it: often while a reply runs or something docked counts by the second, rarely
// otherwise, and at once when a reply starts or ends
func (c *chat) docking(ctx context.Context, id string, epoch int) {
	docks := c.desk.docks
	if len(docks) == 0 || id == "" {
		return
	}
	for {
		var docked []toolsapp.Docked
		live := false
		for _, dock := range docks {
			if shown, err := dock.Load(ctx, id); err == nil && shown != nil {
				docked = append(docked, shown)
				live = live || shown.Live(time.Now())
			}
		}
		if ctx.Err() != nil {
			return
		}
		c.kit.Apply(func() tea.Cmd {
			if epoch == c.epoch {
				c.docked = docked
			}
			return nil
		})
		every := dockIdleEvery
		if live || c.working.Load() {
			every = dockEvery
		}
		select {
		case <-ctx.Done():
			return
		case <-c.poke:
		case <-time.After(every):
		}
	}
}

// Has the docks read again at once
func (c *chat) nudge() {
	select {
	case c.poke <- struct{}{}:
	default:
	}
}

// Rows the docks show above the prompt of a chat of the size, the rows they list split between them
func (c *chat) dock(w, h int) []string {
	limit := max(1, min(dockMaxRows, (h-10)/6))
	// The subagents working now come first, then what the plugins dock
	rows := c.agentDock(w, max(1, limit/2))
	if len(c.docked) == 0 {
		return rows
	}
	share := max(1, limit/len(c.docked))
	now := time.Now()
	for _, docked := range c.docked {
		rows = append(rows, docked.Rows(w, share, now, c.kit.Env.Theme, c.folded)...)
	}
	return rows
}
