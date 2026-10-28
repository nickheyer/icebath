package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Hook that answers a repeated read only call whose result did not change by pointing at the earlier result
type Dedup struct {
	Hooks
	Session *session.Session
	Tools   *tool.Registry
}

func (d *Dedup) AfterTool(ctx context.Context, call protocol.ToolCall, result protocol.ToolResult) (protocol.ToolResult, error) {
	if d.Session == nil || d.Tools == nil || result.IsError || !d.readOnly(call.Name) {
		return result, nil
	}
	body, err := session.MarshalParts(result.Content)
	if err != nil {
		return result, nil
	}
	args := compactJSON(call.Args)
	matching := map[string]bool{}
	earlier := ""
	for _, e := range d.Session.Entries() {
		for _, p := range e.Content {
			switch v := p.(type) {
			case protocol.ToolCall:
				if v.ID != call.ID && v.Name == call.Name && bytes.Equal(compactJSON(v.Args), args) {
					matching[v.ID] = true
				}
			case protocol.ToolResult:
				if !matching[v.CallID] || v.IsError {
					continue
				}
				if _, pruned := d.Session.Pruned(v.CallID); pruned {
					continue
				}
				if stored, err := session.MarshalParts(v.Content); err == nil && bytes.Equal(stored, body) {
					earlier = v.CallID
				}
			}
		}
	}
	if earlier == "" {
		return result, nil
	}
	text := fmt.Sprintf("[unchanged since the earlier %s call %s with the same arguments, its result above still applies]", call.Name, earlier)
	return protocol.ToolResult{CallID: result.CallID, Name: result.Name, Content: []protocol.Part{protocol.Text{Text: text}}}, nil
}

// Reports whether the named tool declares itself read only
func (d *Dedup) readOnly(name string) bool {
	t, ok := d.Tools.Lookup(name)
	if !ok {
		return false
	}
	a, ok := t.(tool.Annotated)
	return ok && a.Annotations().ReadOnly
}

// Arguments with insignificant whitespace removed, the raw bytes when they are not JSON
func compactJSON(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

var _ Hook = (*Dedup)(nil)
