package facts

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Name the model calls to record a finding
const ToolName = "Record"

const schema = `{"type":"object","properties":{
"title":{"type":"string","description":"One line stating the finding"},
"body":{"type":"string","description":"Details, reasoning and what was checked"},
"kind":{"type":"string","description":"One of finding, risk, decision, question or note"},
"tags":{"type":"array","items":{"type":"string"},"description":"Short labels for filtering"},
"confidence":{"type":"number","minimum":0,"maximum":1,"description":"Belief from 0 to 1, use 1 only for verified facts"},
"evidence":{"type":"array","items":{"type":"object","properties":{
 "kind":{"type":"string","description":"file, url, command or tool"},
 "ref":{"type":"string","description":"Path, URL, command line or tool call id"},
 "line":{"type":"integer","description":"Line number inside a file"},
 "quote":{"type":"string","description":"Passage copied from the source"}},"required":["ref"],"additionalProperties":false}},
"id":{"type":"string","description":"Id of an existing fact to update instead of adding a new one"},
"status":{"type":"string","description":"open, resolved or retracted"},
"parent":{"type":"string","description":"Id of the fact this one refines"}},
"required":["title"],"additionalProperties":false}`

// Tool that lets an agent record, refine and resolve facts in the store
type Tool struct {
	Store *Store
	// Kinds accepted, DefaultKinds when empty
	Kinds []string
}

func (t Tool) Definition() protocol.Tool {
	kinds := t.Kinds
	if len(kinds) == 0 {
		kinds = DefaultKinds
	}
	return protocol.Tool{
		Name:        ToolName,
		Description: fmt.Sprintf("Keeps a conclusion the user wants after the session, such as a bug found, a risk, a decision or an open question, beside the transcript. Routine command output and progress belong in prose instead. Kinds are %s. Cite evidence with file paths and line numbers. Give an existing id to refine or resolve a fact.", strings.Join(kinds, ", ")),
		Parameters:  json.RawMessage(schema),
	}
}

func (Tool) Annotations() tool.Annotations {
	return tool.Annotations{Idempotent: true}
}

func (t Tool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	if t.Store == nil {
		return tool.Error("facts: no store configured"), nil
	}
	var in struct {
		ID         string     `json:"id"`
		Title      string     `json:"title"`
		Body       string     `json:"body"`
		Kind       string     `json:"kind"`
		Tags       []string   `json:"tags"`
		Confidence *float64   `json:"confidence"`
		Evidence   []Evidence `json:"evidence"`
		Status     string     `json:"status"`
		Parent     string     `json:"parent"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	kinds := t.Kinds
	if len(kinds) == 0 {
		kinds = DefaultKinds
	}
	if in.Kind != "" && !allowed(kinds, in.Kind) {
		return tool.Errorf("kind %q is not one of %s", in.Kind, strings.Join(kinds, ", ")), nil
	}
	f := Fact{ID: in.ID, Agent: policy.AgentFrom(ctx), Kind: in.Kind, Title: in.Title, Body: in.Body, Tags: in.Tags, Evidence: in.Evidence, Status: in.Status, Parent: in.Parent, Confidence: 0.5}
	if in.Confidence != nil {
		f.Confidence = *in.Confidence
	}
	var out Fact
	var err error
	verb := "Recorded"
	if in.ID != "" {
		if old, ok := t.Store.Get(in.ID); ok {
			if in.Confidence == nil {
				f.Confidence = old.Confidence
			}
			if in.Kind == "" {
				f.Kind = old.Kind
			}
			if in.Body == "" {
				f.Body = old.Body
			}
			if len(in.Tags) == 0 {
				f.Tags = old.Tags
			}
			if len(in.Evidence) == 0 {
				f.Evidence = old.Evidence
			}
			if in.Status == "" {
				f.Status = old.Status
			}
			if in.Parent == "" {
				f.Parent = old.Parent
			}
			out, err = t.Store.Update(f)
			verb = "Updated"
		} else {
			out, err = t.Store.Add(f)
		}
	} else {
		out, err = t.Store.Add(f)
	}
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Textf("%s %s %s: %s (%d facts on record)", verb, out.Kind, out.ID, out.Title, t.Store.Len()).With(out), nil
}

func allowed(kinds []string, kind string) bool {
	for _, k := range kinds {
		if strings.EqualFold(k, kind) {
			return true
		}
	}
	return false
}

var _ tool.Tool = Tool{}
