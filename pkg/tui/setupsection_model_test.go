package tui

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Gateway that answers each method from a queue of replies and records every request
type setupFlowGateway struct {
	replies map[string][]map[string]any
	calls   []map[string]any
}

func (g *setupFlowGateway) request(_ context.Context, method string, params map[string]any, _ time.Duration) (any, error) {
	call := map[string]any{"method": method}
	for k, v := range params {
		call[k] = v
	}
	g.calls = append(g.calls, call)
	queue := g.replies[method]
	if len(queue) == 0 {
		return map[string]any{}, nil
	}
	g.replies[method] = queue[1:]
	return queue[0], nil
}

func (g *setupFlowGateway) canonical() bool { return false }

// Requests of one method in the order they were sent
func (g *setupFlowGateway) sent(method string) []map[string]any {
	var out []map[string]any
	for _, c := range g.calls {
		if c["method"] == method {
			out = append(out, c)
		}
	}
	return out
}

// App whose setup flow verbs answer from the script
func hostedApp(t *testing.T, replies map[string][]map[string]any) (*app, *setupFlowGateway) {
	t.Helper()
	a := testApp(t)
	gw := &setupFlowGateway{replies: replies}
	a.events.gw = gw
	return a, gw
}

func question(id int, kind, title string, extra map[string]any) map[string]any {
	q := map[string]any{"id": float64(id), "kind": kind, "title": title, "description": "", "rows": []any{}, "headings": []any{},
		"default": float64(0), "picked": []any{}, "argv": []any{}}
	for k, v := range extra {
		q[k] = v
	}
	return map[string]any{"lines": []any{}, "question": q, "done": false, "ok": false, "model": "", "provider": "", "changed": false}
}

func TestHostedChoiceOpensAPaletteAtTheDefaultRowAndSendsThePick(t *testing.T) {
	a, gw := hostedApp(t, map[string][]map[string]any{
		"setup.hosted.start": {{"job": "flow-1"}},
		"setup.hosted.poll": {
			{"lines": []any{"Current model:    (not set)"}, "question": nil, "done": false, "ok": false, "model": "", "provider": "", "changed": false},
			question(1, "choice", "Select provider:", map[string]any{"rows": []any{"Anthropic", "OpenRouter", "Vision        (image analysis)       deepseek · v4"}, "default": float64(1),
				"headings": []any{map[string]any{"at": float64(2), "text": "Other"}}}),
			{"lines": []any{"Default model set to: m1"}, "question": nil, "done": true, "ok": true, "model": "m1", "provider": "openrouter", "changed": true},
		},
	})
	drain(a, a.openHostedModel(""))
	p, isPalette := a.wm.Overlay().(*wm.Palette)
	if !isPalette {
		t.Fatalf("overlay %T", a.wm.Overlay())
	}
	items := p.Matches()
	if len(items) != 3 || items[0].Title != "OpenRouter" || items[2].Title != "Vision" || items[2].Detail != "Other · (image analysis) · deepseek · v4" ||
		p.Placeholder != "Select provider" {
		t.Fatalf("items %+v placeholder %q", items, p.Placeholder)
	}
	drain(a, items[0].Run())
	answers := gw.sent("setup.hosted.answer")
	if len(answers) != 1 || answers[0]["index"] != int64(1) || answers[0]["question"] != int64(1) || answers[0]["job"] != "flow-1" {
		t.Fatalf("answers %v", answers)
	}
	got := screen(a)
	for _, want := range []string{"Current model:    (not set)", "Default model set to: m1", "default model m1 · openrouter"} {
		if !strings.Contains(got, want) {
			t.Fatalf("screen lacks %q:\n%s", want, got)
		}
	}
}

func TestHostedSecretAndEscapeAnswerAsBackedOut(t *testing.T) {
	a, gw := hostedApp(t, map[string][]map[string]any{
		"setup.hosted.start": {{"job": "flow-2"}},
		"setup.hosted.poll": {question(4, "secret", "DEEPSEEK_API_KEY (or Enter to cancel):", nil),
			{"lines": []any{"Cancelled."}, "question": nil, "done": true, "ok": true, "model": "", "provider": "", "changed": false}},
	})
	drain(a, a.openHostedModel("deepseek"))
	if start := gw.sent("setup.hosted.start"); len(start) != 1 || start[0]["entry"] != "deepseek" || start[0]["flow"] != "model" {
		t.Fatalf("start %v", start)
	}
	d, isDialog := a.wm.Overlay().(*dialogCard)
	if !isDialog || len(d.fields) != 1 || !d.fields[0].masked || d.title != "DEEPSEEK_API_KEY (or Enter to cancel)" {
		t.Fatalf("overlay %T %+v", a.wm.Overlay(), d)
	}
	_, cmd := a.Update(keys.Press("esc"))
	drain(a, cmd)
	answers := gw.sent("setup.hosted.answer")
	if len(answers) != 1 || answers[0]["cancelled"] != true || answers[0]["question"] != int64(4) {
		t.Fatalf("answers %v", answers)
	}
	if !strings.Contains(screen(a), "Cancelled.") {
		t.Fatalf("screen %s", screen(a))
	}
}

func TestHostedChecklistAndTextAnswers(t *testing.T) {
	a, gw := hostedApp(t, map[string][]map[string]any{"setup.hosted.start": {{"job": "flow-3"}}})
	f := &hostedFlow{job: "flow-3"}
	checklist, _ := tuigateway.SetupHostedPoll.Result(question(7, "checklist", "Tools to set up", map[string]any{"rows": []any{"web", "image", "tts"},
		"picked": []any{float64(0), float64(2)}}))
	drain(a, a.askHosted(f, checklist.Question))
	d, isDialog := a.wm.Overlay().(*dialogCard)
	if !isDialog || !d.multi || !reflect.DeepEqual(d.marks, map[int]bool{0: true, 2: true}) {
		t.Fatalf("checklist %T %+v", a.wm.Overlay(), d)
	}
	d.marks[1] = true
	drain(a, d.answer())
	answers := gw.sent("setup.hosted.answer")
	if len(answers) != 1 || !reflect.DeepEqual(answers[0]["picked"], []any{int64(0), int64(1), int64(2)}) {
		t.Fatalf("checklist answer %v", answers)
	}
	prompt, _ := tuigateway.SetupHostedPoll.Result(question(8, "text", "Base URL [https://api.example.com/v1]:", nil))
	drain(a, a.askHosted(f, prompt.Question))
	p, isPalette := a.wm.Overlay().(*wm.Palette)
	if !isPalette || p.Placeholder != "Base URL [https://api.example.com/v1]" || p.OnText == nil {
		t.Fatalf("text prompt %T", a.wm.Overlay())
	}
	drain(a, p.OnText(""))
	if answers = gw.sent("setup.hosted.answer"); len(answers) != 2 || answers[1]["question"] != int64(8) || answers[1]["cancelled"] != nil {
		t.Fatalf("text answer %v", answers)
	}
}

func TestHostedWaitDialogKeepsWaitingOrStopsTheFlow(t *testing.T) {
	idle := map[string]any{"lines": []any{}, "question": nil, "done": false, "ok": false, "model": "", "provider": "", "changed": false,
		"progress": "downloading 40%"}
	a, gw := hostedApp(t, map[string][]map[string]any{
		"setup.hosted.start": {{"job": "flow-4"}},
		"setup.hosted.poll":  {{"lines": []any{"2. Enter this code:", "CODX-5678"}, "question": nil, "done": false, "ok": false, "model": "", "provider": "", "changed": false}, idle, idle, idle, idle, idle, idle, idle, idle},
	})
	drain(a, a.openHostedModel(""))
	d, isDialog := a.wm.Overlay().(*dialogCard)
	if !isDialog || d.title != tr(txt.SetupFlowFlowWaitTitle) || !strings.Contains(strings.Join(d.body, "\n"), "CODX-5678") || len(d.choices) != 2 ||
		d.body[len(d.body)-1] != "downloading 40%" {
		t.Fatalf("wait dialog %T %+v", a.wm.Overlay(), d)
	}
	drain(a, d.pick("wait"))
	again, stillWaiting := a.wm.Overlay().(*dialogCard)
	if !stillWaiting || again == d || len(gw.sent("setup.hosted.cancel")) != 0 {
		t.Fatalf("keep waiting stopped the flow %T", a.wm.Overlay())
	}
	drain(a, again.pick("stop"))
	if cancels := gw.sent("setup.hosted.cancel"); len(cancels) != 1 || cancels[0]["job"] != "flow-4" {
		t.Fatalf("cancel %v", cancels)
	}
}

func TestCommandAnswerReportsHowTheProgramEnded(t *testing.T) {
	if got := commandAnswer(nil); !got.Ran {
		t.Fatalf("clean exit %v", got)
	}
	if got := commandAnswer(exec.Command("false").Run()); !got.Ran {
		t.Fatalf("non zero exit %v", got)
	}
	if got := commandAnswer(exec.Command("sh", "-c", "kill -INT $$").Run()); got.Ran {
		t.Fatalf("interrupted %v", got)
	}
	if got := commandAnswer(errors.New("exec: \"claude\": executable file not found in $PATH")); !strings.Contains(got.Error, "not found") {
		t.Fatalf("start failure %v", got)
	}
}
