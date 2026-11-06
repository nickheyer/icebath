package updaterecord

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
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

func TestReceiptEmbeddedPMSectionsKeepPMInsertionOrder(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	token := pm.BeginReceipt(ctx, "sync")
	pm.RecordStep(ctx, "uv-sync", true, "naïve")
	pm.RecordVenvRebuild(ctx, true, "stale")
	pm.RecordWarning(ctx, "slow mirror")
	pm.RecordRefusal(ctx, "lazy_installs_disabled", "policy")
	pm.FinalizeReceipt(ctx, "refused", 3, token)
	path := FinalizeUpdateReceipt(ctx, "success", nil, "")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		"\"pm_venv_rebuild\": {\n    \"ok\": true,\n    \"reason\": \"stale\"\n  }",
		"\"pm_steps\": [\n    {\n      \"name\": \"uv-sync\",\n      \"ok\": true,\n      \"detail\": \"na\\u00efve\",\n      \"at\": ",
		"\"pm_warnings\": [\n    {\n      \"message\": \"slow mirror\",\n      \"at\": ",
		"\"pm_refusal\": {\n    \"code\": \"lazy_installs_disabled\",\n    \"detail\": \"policy\",\n    \"at\": ",
		"\"pm_exit_code\": 3,\n  \"pm_sync_outcome\": \"refused\",",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	doc := v.(*pyjson.Object)
	var pmKeys []string
	for _, k := range doc.Keys {
		if strings.HasPrefix(k, "pm_") {
			pmKeys = append(pmKeys, k)
		}
	}
	if !reflect.DeepEqual(pmKeys, []string{"pm_venv_rebuild", "pm_steps", "pm_exit_code", "pm_sync_outcome", "pm_warnings", "pm_refusal"}) {
		t.Fatalf("pm keys %v", pmKeys)
	}
	again, err := pyjson.Dumps(doc, pyjson.Indent(2))
	if err != nil || again+"\n" != text {
		t.Fatalf("bytes differ from json.dumps(record, indent=2) + newline: %v", err)
	}
}

func TestReceiptWorkerHandOffKeepsPMInsertionOrder(t *testing.T) {
	ctx, _ := rcHome(t)
	BeginUpdateReceipt(ctx, nil, "")
	id := *CurrentCorrelationID(ctx)
	worker := pyjson.ObjectOf("schema", 1, "kind", "sync", "update_id", id, "venv_rebuild",
		pyjson.ObjectOf("reason", "kept", "ok", false), "outcome", "ok", "exit_code", 0)
	if err := pm.AcceptWorkerReceipt(ctx, worker, &id); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(FinalizeUpdateReceipt(ctx, "success", nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\"pm_venv_rebuild\": {\n    \"reason\": \"kept\",\n    \"ok\": false\n  }") {
		t.Fatalf("worker section reordered\n%s", raw)
	}
}
