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
