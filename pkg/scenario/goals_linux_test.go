package scenario

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

const goalsJudgePrompt = "You are a strict judge evaluating whether an autonomous agent has achieved a user's stated goal."

const goalsTerminalConfig = "display:\n  interface: cli\ntelemetry:\n  shared_metrics:\n    enabled: false\n    send: false\n    offer_version: 2\nupdates:\n  check: false\n"

// Answers the goal judge request with verdict and every other auxiliary request with a title
func goalsJudgeAnswer(verdict string) func(*fakellm.Request) any {
	return func(r *fakellm.Request) any {
		if goalsJudgeRequest(r.Body) {
			return fakellm.Text{Text: verdict}
		}
		return fakellm.Text{Text: "River haiku"}
	}
}

// Reports a request body whose system message is the goal judge prompt
func goalsJudgeRequest(body map[string]any) bool {
	system, _ := Dig(body, "messages", 0, "content").(string)
	return strings.HasPrefix(system, goalsJudgePrompt)
}

// The goal row the session stored in state_meta, decoded
func goalsStoredGoal(s *S) map[string]any {
	rows := s.Rows("state.db", "SELECT key, value FROM state_meta WHERE key LIKE 'goal:%'")
	if len(rows) != 1 {
		s.Failf("state_meta holds %d goal rows, want 1: %v", len(rows), rows)
	}
	var goal map[string]any
	raw, _ := rows[0]["value"].(string)
	if err := json.Unmarshal([]byte(raw), &goal); err != nil {
		s.Failf("the stored goal is not JSON: %q", raw)
	}
	return goal
}

// A goal set on a terminal runs its turn and ends when the judge answers done
func TestScenarioGoalJudgedDoneOnATerminal(t *testing.T) {
	s := Start(t, Setup{
		Model:        []any{fakellm.Text{Text: "Here is the haiku about rivers you asked for."}},
		ModelOptions: fakellm.Options{Aux: goalsJudgeAnswer(`{"verdict": "done", "reason": "the haiku is written"}`)},
		Extra:        goalsTerminalConfig,
	})
	tm := s.Terminal()
	tm.Await("fake-model")
	tm.Line("/goal write a haiku about rivers")
	tm.Await("Here is the haiku about rivers")
	tm.Await("Goal achieved: the haiku is written")
	tm.Line("/quit")
	tm.Await("Resume this session with")
	if code := tm.Wait(); code != 0 {
		s.Failf("the interactive CLI exited %d after /quit\n%s", code, clip(tm.Text()))
	}
	if n := len(s.Model.MainRequests()); n != 1 {
		s.Failf("a done verdict queued more turns: the model saw %d main requests", n)
	}
	judged := 0
	for _, body := range s.Model.AuxRequests() {
		if !goalsJudgeRequest(body) {
			continue
		}
		judged++
		user, _ := Dig(body, "messages", 1, "content").(string)
		if !strings.Contains(user, "Goal:\nwrite a haiku about rivers\n\nAgent's most recent response:\nHere is the haiku about rivers you asked for.\n\n") {
			s.Failf("the judge prompt lacks the goal and the reply\n%s", user)
		}
		if Dig(body, "temperature") != float64(0) {
			s.Failf("the judge ran at temperature %v, want 0", Dig(body, "temperature"))
		}
	}
	if judged != 1 {
		s.Failf("the judge was asked %d times, want 1", judged)
	}
	s.HasJSON(goalsStoredGoal(s), `{"goal": "write a haiku about rivers", "status": "done", "turns_used": 1, "last_verdict": "done", "last_reason": "the haiku is written"}`)
}

// A judge that never answers with a JSON verdict pauses the goal after three turns
func TestScenarioGoalPausesWhenTheJudgeAnswersNoVerdict(t *testing.T) {
	s := Start(t, Setup{
		Model: []any{
			fakellm.Text{Text: "First verse about the river."},
			fakellm.Text{Text: "Second verse about the river."},
			fakellm.Text{Text: "Third verse about the river."},
		},
		ModelOptions: fakellm.Options{Aux: goalsJudgeAnswer("I think it might be finished, hard to say.")},
		Extra:        goalsTerminalConfig,
	})
	tm := s.Terminal()
	tm.Await("fake-model")
	tm.Line("/goal write a haiku about rivers")
	tm.Await(`Continuing toward goal \(1/20\)`)
	tm.Await(`Continuing toward goal \(2/20\)`)
	tm.Await(`Goal paused .+ the judge model \(3 turns\) isn't returning the required JSON verdict`)
	tm.Await(`goal_judge:\s+provider: openrouter\s+model: google/gemini-3-flash-preview\s+Then /goal resume to continue`)
	tm.Line("/quit")
	tm.Await("Resume this session with")
	if code := tm.Wait(); code != 0 {
		s.Failf("the interactive CLI exited %d after /quit\n%s", code, clip(tm.Text()))
	}
	if n := len(s.Model.MainRequests()); n != 3 {
		s.Failf("the paused goal ran %d turns, want 3", n)
	}
	last, _ := Dig(s.Model.MainRequests()[2], "messages", -1, "content").(string)
	if !strings.HasPrefix(last, "[Continuing toward your standing goal]\nGoal: write a haiku about rivers\n") {
		s.Failf("the continuation prompt differs\n%s", last)
	}
	s.HasJSON(goalsStoredGoal(s), `{"goal": "write a haiku about rivers", "status": "paused", "turns_used": 3, "consecutive_parse_failures": 3, "paused_reason": "judge model returned unparseable output 3 turns in a row"}`)
}
