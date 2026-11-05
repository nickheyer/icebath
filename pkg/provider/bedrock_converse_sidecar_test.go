package provider

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestBedrockTransportSendsNoTopPOrStopButCallConverseDoes(t *testing.T) {
	f := bcCallConverseFake(t, newBedrockTurn(bedrockTextBlock("one")), newBedrockTurn(bedrockTextBlock("two")))
	client, err := NewClient(Route{Provider: "bedrock", Model: "us.amazon.nova-pro-v1:0", Mode: modeBedrockConverse, Region: "us-east-1", APIKey: "aws-sdk"})
	if err != nil {
		t.Fatal(err)
	}
	req := &Request{Messages: []*state.Message{{Role: "user", Content: "hi"}}, MaxTokens: 64,
		Temperature: bcFloat(0.3), TopP: bcFloat(0.9), Stop: []string{"END"}}
	if _, err := client.Complete(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	bcSameJSON(t, f.snapshot()[0].Body["inferenceConfig"], `{"maxTokens":64,"temperature":0.3}`)
	in := newBedrockConverseInput("us.amazon.nova-pro-v1:0", []map[string]any{{"role": "user", "content": "hi"}})
	in.Temperature, in.TopP, in.StopSequences = bcFloat(0.3), bcFloat(0.9), []string{"END"}
	if _, err := callBedrockConverse(t.Context(), "us-east-1", in); err != nil {
		t.Fatal(err)
	}
	bcSameJSON(t, f.snapshot()[1].Body["inferenceConfig"], `{"maxTokens":4096,"temperature":0.3,"topP":0.9,"stopSequences":["END"]}`)
}

func TestBedrockContentBlocksSidecarIsLiveOnlyAcrossASessionReload(t *testing.T) {
	tool := &bedrockToolUse{Name: "read_file", Input: map[string]any{"path": "a.txt"}, ToolUseID: "tooluse_1"}
	f := bcCallConverseFake(t, newBedrockTurn(bedrockTextBlock("checking"), tool))
	client, err := NewClient(Route{Provider: "bedrock", Model: "us.amazon.nova-pro-v1:0", Mode: modeBedrockConverse, Region: "us-east-1", APIKey: "aws-sdk"})
	if err != nil {
		t.Fatal(err)
	}
	history := []*state.Message{{Role: "user", Content: "read a.txt"}}
	resp, err := client.Complete(t.Context(), &Request{Messages: history, MaxTokens: 64})
	if err != nil || len(resp.Message.ToolCalls) != 1 || f.snapshot()[0].Rejected != "" {
		t.Fatalf("%+v %v", resp, err)
	}
	if strings.Contains(string(resp.Message.ReasoningDetails), "content_blocks") {
		t.Fatalf("sidecar reached the persisted reasoning details %s", resp.Message.ReasoningDetails)
	}
	live := bedrockMessageDicts([]*state.Message{history[0], resp.Message})
	blocks, ok := asList(live[1]["bedrock_content_blocks"])
	if !ok || len(blocks) != 2 {
		t.Fatalf("live message lost its ordered blocks %v", live[1])
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessages("s1", []*state.Message{history[0], resp.Message}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := db.AllMessages("s1")
	if err != nil || len(reloaded) != 2 {
		t.Fatal(reloaded, err)
	}
	dicts := bedrockMessageDicts(reloaded)
	if _, has := dicts[1]["bedrock_content_blocks"]; has {
		t.Fatalf("reloaded message carries the live only sidecar %v", dicts[1])
	}
	_, turns := convertMessagesToConverse(dicts, nil, "us.amazon.nova-pro-v1:0")
	bcSameJSON(t, turns[1].(map[string]any)["content"],
		`[{"text":"checking"},{"toolUse":{"toolUseId":"tooluse_1","name":"read_file","input":{"path":"a.txt"}}}]`)
}
