package provider

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestBedrockConverseClientStreamsAndCompletesThroughTheFake(t *testing.T) {
	f := bcCallConverseFake(t, newBedrockTurn(bedrockTextBlock("hello from the stream")), newBedrockTurn(bedrockTextBlock("plain answer")))
	client, err := NewClient(Route{Provider: "bedrock", Model: "us.amazon.nova-pro-v1:0", Mode: modeBedrockConverse, Region: "us-east-1", APIKey: "aws-sdk"})
	if err != nil {
		t.Fatal(err)
	}
	req := &Request{Messages: []*state.Message{{Role: "system", Content: "Be brief."}, {Role: "user", Content: "hi"}}, MaxTokens: 64}
	var text string
	var kinds []EventKind
	resp, err := client.Stream(t.Context(), req, func(ev StreamEvent) error {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == EventTextDelta {
			text += ev.Text
		}
		return nil
	})
	if err != nil || text != "hello from the stream" || resp.Message.Text() != "hello from the stream" || resp.FinishReason != "stop" || kinds[len(kinds)-1] != EventDone {
		t.Fatalf("%+v %q %v %v", resp, text, kinds, err)
	}
	recs := f.snapshot()
	if recs[0].Op != "ConverseStream" || recs[0].Model != "us.amazon.nova-pro-v1:0" || recs[0].Rejected != "" {
		t.Fatalf("%+v", recs[0])
	}
	history := append(req.Messages, resp.Message, &state.Message{Role: "user", Content: "again"})
	dicts := bedrockMessageDicts(history)
	bcSameJSON(t, dicts[2]["bedrock_content_blocks"], `[{"text":"hello from the stream"}]`)
	if _, has := dicts[2]["reasoning_details"]; has {
		t.Fatal(dicts[2])
	}
	final, err := client.Complete(t.Context(), &Request{Messages: history, MaxTokens: 64})
	if err != nil || final.Message.Text() != "plain answer" || f.snapshot()[1].Op != "Converse" {
		t.Fatalf("%+v %v", final, err)
	}
	if n, err := client.CountTokens(t.Context(), req); err != nil || n <= 0 {
		t.Fatal(n, err)
	}
}
