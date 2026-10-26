// Extension that works an objective in iterations until the agent ends its answer with DONE
package loop

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/ext"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Line the agent ends with once the objective is met
const Mark = "DONE"

const guidance = `# Loop
You are run in a loop toward one objective. Each iteration is a new turn in the same session, so your earlier work stands in the transcript. Keep the plan current with Plan. Verify the work before claiming it is complete. When the objective is fully met and verified, end your message with a final line that is exactly ` + Mark + `. Otherwise end with what remains, and the loop will prompt you to continue.`

type loop struct{}

func init() { ext.Register(loop{}) }

func (loop) Manifest() ext.Manifest {
	return ext.Manifest{Name: "loop", Version: "1.0.0", Description: "works an objective in iterations until the agent ends with " + Mark}
}

func (loop) Attach(h *ext.Host) error {
	h.Kind(harness.Kind{Name: "loop", Description: "works one objective across iterations and ends with " + Mark + " once it is met", Append: guidance})
	h.Setting(harness.Setting{Key: "loop_max", Help: "Iterations a loop runs before it gives up, unset for no limit", Lo: 1, Step: 1, Integer: true})
	h.Workflow(harness.Workflow{Name: "loop", Title: "loop an objective", Description: "work an objective in iterations until the agent ends with " + Mark, Arg: "objective", Run: run})
	return nil
}

// Runs the objective on a loop session until the agent ends with the mark or the iterations run out
func run(ctx context.Context, f *harness.Flow, objective string) error {
	limit, _ := strconv.Atoi(f.App.Config.Setting("loop_max"))
	live, err := seat(ctx, f)
	if err != nil {
		return err
	}
	ms, err := live.Missions.Create(objective)
	if err != nil {
		return err
	}
	prompt := objective
	for i := 1; limit <= 0 || i <= limit; i++ {
		f.SetStatus(progress(i, limit))
		r, err := f.Start(ctx, live, ms.ID, []protocol.Part{protocol.Text{Text: prompt}})
		if err != nil {
			return end(live, ms.ID, mission.StatusFailed, err)
		}
		resp, err := f.Wait(ctx, r)
		if err != nil {
			if ctx.Err() != nil {
				return end(live, ms.ID, mission.StatusCancelled, err)
			}
			return end(live, ms.ID, mission.StatusFailed, err)
		}
		if done(resp.Text()) {
			f.Notify(harness.Success, "loop done at "+progress(i, limit))
			return end(live, ms.ID, mission.StatusDone, nil)
		}
		f.Notify(harness.Info, fmt.Sprintf("loop %s ended without %s", progress(i, limit), Mark))
		prompt = fmt.Sprintf("This is %s. The objective is not yet reported done. Continue from where you left off, verify the work, and end with %s only once the objective is fully met.", progress(i+1, limit), Mark)
	}
	return end(live, ms.ID, mission.StatusFailed, fmt.Errorf("loop: %d iterations without %s", limit, Mark))
}

// Words for the iteration, with the limit when there is one
func progress(i, limit int) string {
	if limit > 0 {
		return fmt.Sprintf("iteration %d of %d", i, limit)
	}
	return fmt.Sprintf("iteration %d", i)
}

// Session the loop runs on, the one it was started from when idle and empty, else a fresh one of the loop kind
func seat(ctx context.Context, f *harness.Flow) (*harness.Live, error) {
	if l := f.Live; l != nil && l.Session.Len() == 0 && !f.App.Busy(l) {
		return l, f.App.SetKindOn(l, "loop")
	}
	return f.Open(ctx, "loop")
}

// Closes the mission with the outcome and hands the error back
func end(live *harness.Live, id, status string, err error) error {
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	if _, ferr := live.Missions.Finish(id, status, reason); ferr != nil && err == nil {
		return ferr
	}
	return err
}

// Reports whether the last line of the answer is the mark
func done(text string) bool {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	return strings.Trim(last, "*`_ ") == Mark
}
