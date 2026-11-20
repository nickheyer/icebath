package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestWorkerAutomationCodecsRewriteStatusThroughTheRealStateTypes(t *testing.T) {
	rows := map[string]string{
		"goal":      `{"goal": "retain objective", "status": "paused"}`,
		"heartbeat": `{"status": "active", "interval_seconds": 120}`,
		"loop":      `{"prompt": "retain loop", "status": "active", "interval_seconds": 60}`,
	}
	for family, raw := range rows {
		codec, ok := state.WorkerAutomationCodecs[family]
		if !ok {
			t.Fatalf("%s codec not installed", family)
		}
		st, err := codec(raw)
		if err != nil {
			t.Fatal(err)
		}
		decoded, _ := pyjson.Loads(raw)
		want, _ := decoded.(*pyjson.Object).Get("status")
		if !pyjson.Equal(st.Status(), want) {
			t.Fatalf("%s status %v != %v", family, st.Status(), want)
		}
		st.SetStatus("cleared")
		text, err := st.ToJSON()
		if err != nil {
			t.Fatal(err)
		}
		again, _ := pyjson.Loads(text)
		if status, _ := again.(*pyjson.Object).Get("status"); status != "cleared" {
			t.Fatalf("%s re encoded status %v", family, status)
		}
		if _, err := codec("not json"); err == nil {
			t.Fatalf("%s accepted invalid JSON", family)
		}
	}
}
