package tools

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCliResumeDisengages(t *testing.T) {
	ctx := context.Background()
	estopHome(t)
	Engage(ctx, "")
	var out bytes.Buffer
	code, err := ResumeCommand(ctx, "", nil, &out)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if IsEngaged(ctx) {
		t.Fatal("still engaged after resume")
	}
	if !strings.Contains(strings.ToLower(out.String()), "resumed") {
		t.Fatalf("output %q", out.String())
	}
}

func TestCliResumeWithoutAPauseNamesTheSentinel(t *testing.T) {
	ctx := context.Background()
	dir := estopHome(t)
	var out bytes.Buffer
	if code, err := ResumeCommand(ctx, dir, nil, &out); err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if !strings.Contains(out.String(), "Icebath is not paused (no sentinel at "+SentinelPath(ctx)+").") {
		t.Fatalf("output %q", out.String())
	}
}

func TestCliPauseEngagesWithReason(t *testing.T) {
	ctx := context.Background()
	estopHome(t)
	var out bytes.Buffer
	code, err := PauseCommand(ctx, map[string]any{"reason": "ops incident"}, &out)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if !IsEngaged(ctx) {
		t.Fatal("not engaged after pause")
	}
	if state := GetState(ctx); state == nil || state.Reason != "ops incident" {
		t.Fatalf("state %+v", state)
	}
	if text := out.String(); !strings.Contains(strings.ToLower(text), "paused") || !strings.Contains(text, "⏸️  Icebath paused — reason: ops incident\n    sentinel: "+SentinelPath(ctx)+"\n") {
		t.Fatalf("output %q", text)
	}
}

func TestCliPauseIdempotent(t *testing.T) {
	ctx := context.Background()
	estopHome(t)
	var out bytes.Buffer
	for range 2 {
		if code, err := PauseCommand(ctx, map[string]any{"reason": nil}, &out); err != nil || code != 0 {
			t.Fatalf("code %d err %v", code, err)
		}
	}
	if !IsEngaged(ctx) {
		t.Fatal("not engaged after two pauses")
	}
	if !strings.Contains(out.String(), "⏸️  Still paused\n") {
		t.Fatalf("output %q", out.String())
	}
}
