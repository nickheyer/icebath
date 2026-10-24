package cards

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Markdown answer of about four hundred words with a list, emphasis and a code fence
var benchAnswer = strings.Repeat(`So here you stand, in front of the **tiling desk**, cards breathing. Somewhere a mission objective is burning in a header and a pump is turning, events arriving as JSON lines for anyone who prefers truth without a terminal.

- the models beyond the wall do not know they are being tiled
- they think they are answering a *person*
- they are answering a layout manager

`+"```go\n"+`for _, e := range ws.entries() {
	moving = m.step(e, dt, focused, hovered) || moving
}
`+"```\n\n", 4)

// Transcript holding turns finished answers, each a rendered markdown block
func benchTranscript(turns int) *Transcript {
	env := &Env{Theme: theme.Blackwall(), Keys: keys.Default(), Now: time.Now}
	tr := NewTranscript(env)
	at := time.Now()
	for i := 0; i < turns; i++ {
		tr.Apply(agent.Event{Type: agent.EventRunStart, Time: at, Parts: []protocol.Part{protocol.Text{Text: fmt.Sprintf("question %d", i)}}})
		tr.Apply(agent.Event{Type: agent.EventTurnStart, Time: at})
		resp := protocol.Response{Model: "m", Content: []protocol.Part{protocol.Text{Text: benchAnswer}}}
		tr.Apply(agent.Event{Type: agent.EventModel, Time: at, Model: protocol.Event{Type: protocol.EventTextDelta, Response: resp}})
		tr.Apply(agent.Event{Type: agent.EventTurnEnd, Time: at, Response: &resp})
		tr.Apply(agent.Event{Type: agent.EventRunEnd, Time: at})
	}
	return tr
}

func BenchmarkTranscriptStreaming(b *testing.B) {
	tr := benchTranscript(10)
	tr.View(100, 50)
	at := time.Now()
	tr.Apply(agent.Event{Type: agent.EventRunStart, Time: at, Parts: []protocol.Part{protocol.Text{Text: "tell me a story"}}})
	tr.Apply(agent.Event{Type: agent.EventTurnStart, Time: at})
	words := strings.Fields(benchAnswer)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := min((i%400+1)*3, len(words))
		resp := protocol.Response{Model: "m", Content: []protocol.Part{protocol.Text{Text: strings.Join(words[:n], " ")}}}
		tr.Apply(agent.Event{Type: agent.EventModel, Time: at, Model: protocol.Event{Type: protocol.EventTextDelta, Response: resp}})
		tr.View(100, 50)
	}
}

func BenchmarkTranscriptResize(b *testing.B) {
	tr := benchTranscript(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.View(100+i%2, 50)
	}
}

func BenchmarkTranscriptIdleView(b *testing.B) {
	tr := benchTranscript(10)
	tr.View(100, 50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.View(100, 50)
	}
}
