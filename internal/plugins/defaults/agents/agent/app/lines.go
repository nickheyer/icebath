package app

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	"github.com/nickheyer/icebath/pkg/tui"
)

// A line the person ran in the chat after the mark of a liner, and what it printed so far
type lineRun struct {
	liner *toolsapp.Liner
	ran   toolsapp.Ran
	// Ends it while it runs
	stop func()
	// Fold id of its rows in the transcript
	id string
}

// Liner whose mark starts the text, with the line after the mark; false for text no liner takes
func (d *desk) liner(text string) (*toolsapp.Liner, string, bool) {
	for i := range d.liners {
		liner := &d.liners[i]
		if line, ok := strings.CutPrefix(text, liner.Mark); ok && liner.Mark != "" && strings.TrimSpace(line) != "" {
			return liner, strings.TrimSpace(line), true
		}
	}
	return nil, "", false
}

// Line running now, nil when none is
func (c *chat) running() *lineRun {
	for _, run := range c.runs {
		if !run.ran.Done {
			return run
		}
	}
	return nil
}

// Runs the line in the chat, what it prints shown under it as it comes; one line runs at a time
func (c *chat) runLine(liner *toolsapp.Liner, line string) tea.Cmd {
	c.follow = true
	if c.running() != nil {
		c.log.note("A line is still running. Press " + c.kit.WM.Keys().Display("session.stop") + " to stop it, then run another.")
		return nil
	}
	run := &lineRun{liner: liner, ran: toolsapp.Ran{Time: time.Now(), Line: line}, id: fmt.Sprintf("line:%d", len(c.runs))}
	c.runs = append(c.runs, run)
	epoch := c.epoch
	run.stop = liner.Run(c.kit.Env.Ctx, toolsapp.Run{Session: c.id, Profile: c.on(), Folder: c.kit.Env.WorkDir(), Line: line,
		Shell: c.kit.Env.Shell().Path}, func(ran toolsapp.Ran) {
		c.kit.Apply(func() tea.Cmd {
			if epoch != c.epoch {
				return nil
			}
			at := run.ran.Time
			run.ran = ran
			// The time the line started orders it among the messages, whatever time the server says it ran at
			if at.IsZero() || ran.Time.IsZero() {
				run.ran.Time = at
			}
			if ran.Done {
				run.stop = nil
			}
			return nil
		})
	})
	return nil
}

// Stops the line running now, false when none is
func (c *chat) stopLine() tea.Cmd {
	if run := c.running(); run != nil && run.stop != nil {
		run.stop()
	}
	return nil
}

// Reads the lines run in the session before, placing them among its messages by the time each ran; the line running now stays
func (c *chat) keptLines(epoch int) tea.Cmd {
	id, profile := c.id, c.on()
	if id == "" || len(c.desk.liners) == 0 {
		return nil
	}
	liners := c.desk.liners
	return tui.Async(func() func() tea.Cmd {
		var found []*lineRun
		var failed error
		for i := range liners {
			liner := &liners[i]
			if liner.Kept == nil {
				continue
			}
			kept, err := liner.Kept(c.kit.Env.Ctx, id, profile)
			if err != nil {
				failed = err
				continue
			}
			for _, ran := range kept {
				found = append(found, &lineRun{liner: liner, ran: ran})
			}
		}
		return func() tea.Cmd {
			if epoch != c.epoch || c.id != id {
				return nil
			}
			if failed != nil {
				c.log.note("The lines you ran in this session could not be read. " + tui.Reason(failed))
			}
			var live []*lineRun
			for _, run := range c.runs {
				if !run.ran.Done || !slices.ContainsFunc(found, func(k *lineRun) bool { return k.ran.Line == run.ran.Line && near(k.ran.Time, run.ran.Time) }) {
					live = append(live, run)
				}
			}
			c.runs = append(found, live...)
			slices.SortStableFunc(c.runs, func(a, b *lineRun) int { return a.ran.Time.Compare(b.ran.Time) })
			for i, run := range c.runs {
				run.id = fmt.Sprintf("line:%d", i)
			}
			return nil
		}
	})
}

// Whether two times are those of one line, as the app and the server each took it
func near(a, b time.Time) bool {
	d := a.Sub(b)
	return d < 5*time.Second && d > -5*time.Second
}
