package updaterecord

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestReceiptArchiveBytesFollowDumpsIndentTwoInsertionOrder(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	RecordStep(ctx, "fetch", true, "café ✓")
	RecordStage(ctx, "apply", "success", pyjson.ObjectOf("mode", "binary", "attempt", 2))
	path := FinalizeUpdateReceipt(ctx, "success", nil, "done")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"detail": "caf\u00e9 \u2713"`) || !strings.HasSuffix(text, "}\n") {
		t.Fatalf("not ensure_ascii indent 2 dumps:\n%s", text)
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	doc := v.(*pyjson.Object)
	want := []string{"schema", "started_at", "finished_at", "argv", "pid", "outcome", "pre_update", "post_update",
		"steps", "skips", "gateway_restart", "fleet", "update_id", "correlation_id", "action_id", "pid_create_time",
		"stages", "stop_reason"}
	if !reflect.DeepEqual(doc.Keys, want) {
		t.Fatalf("keys %v", doc.Keys)
	}
	step := doc.Vals["steps"].([]any)[0].(*pyjson.Object)
	stage := doc.Vals["stages"].([]any)[0].(*pyjson.Object)
	if !reflect.DeepEqual(step.Keys, []string{"name", "ok", "detail", "at"}) ||
		!reflect.DeepEqual(stage.Keys, []string{"name", "outcome", "at", "mode", "attempt"}) {
		t.Fatalf("step %v stage %v", step.Keys, stage.Keys)
	}
	again, err := pyjson.Dumps(doc, pyjson.Indent(2))
	if err != nil || again+"\n" != text {
		t.Fatalf("bytes differ from json.dumps(record, indent=2) + newline: %v", err)
	}
}
