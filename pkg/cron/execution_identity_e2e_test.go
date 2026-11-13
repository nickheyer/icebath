package cron_test

import (
	"os"
	"path/filepath"
	"testing"
)

const cronIdentityProbePlugin = `
import dataclasses, json, os
from pathlib import Path

CTX = None

def _record(**kwargs):
    ident = CTX.current_cron_execution()
    out = Path(os.environ["ICEBATH_HOME"]) / "seen.json"
    out.write_text(json.dumps({"tool": kwargs.get("tool_name"),
                               "identity": dataclasses.asdict(ident) if ident else None}))

def register(ctx):
    global CTX
    CTX = ctx
    ctx.register_hook("pre_tool_call", _record)
`

func writeIdentityFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
