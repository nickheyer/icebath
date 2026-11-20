package tuiserver

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/setupflow"
)

// Runs steps until the flow asks or ends, failing the case on an error response
func flowStepUntil(t *testing.T, session string, answer map[string]any) map[string]any {
	t.Helper()
	params := map[string]any{"session": session}
	if answer != nil {
		params["answer"] = answer
	}
	for i := 0; i < 20; i++ {
		res := resultOf(t, contractCall(t, "setup.flow.step", params))
		delete(params, "answer")
		if res["question"] != nil || res["done"] == true || len(res["lines"].([]any)) > 0 {
			return res
		}
	}
	t.Fatal("flow never answered")
	return nil
}

func TestSetupFlowAsksThroughTheStepVerb(t *testing.T) {
	setupSectionHome(t, "model:\n  default: m\n", "")
	got := make(chan []any, 1)
	setupFlows["test.flow"] = func(_ context.Context, _ string, params map[string]string, ask setupflow.Asker) error {
		ask.Say("hello " + params["who"])
		idx, err := ask.Choose("pick", []setupflow.Option{{Title: "a"}, {Title: "b"}}, 1)
		if err != nil {
			return err
		}
		chosen, err := ask.Check("tick", []setupflow.Item{{Title: "x", Tools: []string{"t1"}}, {Title: "y"}}, map[int]bool{1: true}, map[string]int{"t1": 5})
		if err != nil {
			return err
		}
		text, err := ask.Text("name", "anon", false)
		if err != nil {
			return err
		}
		kept, err := ask.Choose("again", []setupflow.Option{{Title: "a"}, {Title: "b"}}, 1)
		got <- []any{idx, chosen, text, kept}
		if err != nil {
			return err
		}
		_, err = ask.Text("last", "", true)
		return err
	}
	t.Cleanup(func() { delete(setupFlows, "test.flow") })
	session := resultOf(t, callMethod(t, "setup.flow.start", map[string]any{"flow": "test.flow", "params": map[string]any{"who": "nick"}}))["session"].(string)
	res := flowStepUntil(t, session, nil)
	if !reflect.DeepEqual(res["lines"], []any{"hello nick"}) && res["question"] == nil {
		t.Fatalf("first step %v", res)
	}
	question, _ := res["question"].(map[string]any)
	if question == nil {
		question = flowStepUntil(t, session, nil)["question"].(map[string]any)
	}
	if question["kind"] != "choose" || question["default"] != 1 {
		t.Fatalf("question %v", question)
	}
	res = flowStepUntil(t, session, map[string]any{"question": question["id"], "index": 0})
	question = res["question"].(map[string]any)
	if question["kind"] != "check" || !reflect.DeepEqual(question["selected"], []any{1}) || !reflect.DeepEqual(question["tokens"], map[string]any{"t1": 5}) {
		t.Fatalf("question %v", question)
	}
	if resp := contractCall(t, "setup.flow.step", map[string]any{"session": session, "answer": map[string]any{"question": 99, "index": 0}}); cfgErrCode(resp) != 4001 {
		t.Fatalf("stale answer accepted %v", resp)
	}
	res = flowStepUntil(t, session, map[string]any{"question": question["id"], "indices": []any{0, 1}})
	question = res["question"].(map[string]any)
	res = flowStepUntil(t, session, map[string]any{"question": question["id"], "text": ""})
	question = res["question"].(map[string]any)
	res = flowStepUntil(t, session, map[string]any{"question": question["id"], "cancel": true})
	if values := <-got; !reflect.DeepEqual(values, []any{0, map[int]bool{0: true, 1: true}, "anon", 1}) {
		t.Fatalf("answers %v", values)
	}
	question = res["question"].(map[string]any)
	res = flowStepUntil(t, session, map[string]any{"question": question["id"], "cancel": true})
	if res["done"] != true || res["ok"] != false || res["error"] != setupflow.ErrCancelled.Error() {
		t.Fatalf("end %v", res)
	}
	if resp := contractCall(t, "setup.flow.step", map[string]any{"session": session}); cfgErrCode(resp) != 4001 {
		t.Fatalf("finished session kept %v", resp)
	}
}
