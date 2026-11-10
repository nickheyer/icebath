package pm

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const relockPyproject = `[project]
name = "demo"
version = "0"

[project.optional-dependencies]
alpha = ["six"]
beta = ["idna"]
all = ["demo[alpha]"]
`

const relockLock = `version = 1

[[package]]
name = "demo"
version = "0"
source = { editable = "." }

[package.metadata]
provides-extras = ["alpha", "all"]
`

func TestRelockWritesNothingWhenCurrentAndNamesNewOptInExtras(t *testing.T) {
	isolateHome(t)
	tmp := t.TempDir()
	peWrite(t, filepath.Join(tmp, "pyproject.toml"), relockPyproject)
	peWrite(t, filepath.Join(tmp, "uv.lock"), relockLock)
	peSwap(t, &repoRoot, func() string { return tmp })
	stale := false
	var relocks []any
	peSwap(t, &pmCheckProjectLock, func(_ context.Context, opts CheckProjectLockOptions) error {
		if opts.Source != tmp || !opts.Explicit {
			t.Errorf("checked %q explicit %v", opts.Source, opts.Explicit)
		}
		if !opts.Quiet {
			t.Error("a stale lock is the expected case and must be checked quietly")
		}
		if stale {
			return NewInstallError("venv", "The lockfile at `uv.lock` needs to be updated", "")
		}
		return nil
	})
	peSwap(t, &pmLockProject, func(_ context.Context, opts LockProjectOptions) error {
		relocks = append(relocks, opts)
		return nil
	})
	peSwap(t, &clientSyncVenv, func(context.Context, SyncVenvOptions) error {
		relocks = append(relocks, "sync")
		return nil
	})
	peSwap(t, &installSyncVenv, func(context.Context, SyncVenvOptions) error {
		relocks = append(relocks, "sync")
		return nil
	})
	before := peRead(t, filepath.Join(tmp, "uv.lock"))
	var out bytes.Buffer
	ctx := withSyncOutput(context.Background(), &out)
	if code, err := CmdLock(ctx, LockArgs{}); code != 0 || err != nil {
		t.Fatalf("current lock %d %v", code, err)
	}
	if len(relocks) != 0 || peRead(t, filepath.Join(tmp, "uv.lock")) != before || !strings.Contains(out.String(), "already current") {
		t.Fatalf("relocks %v out %q", relocks, out.String())
	}
	stale = true
	out.Reset()
	if code, err := CmdLock(ctx, LockArgs{}); code != 0 || err != nil {
		t.Fatalf("stale lock %d %v", code, err)
	}
	if !reflect.DeepEqual(relocks, []any{LockProjectOptions{Source: tmp, Explicit: true}}) {
		t.Fatalf("relocks %+v", relocks)
	}
	text := out.String()
	if !strings.Contains(text, "out of date") || strings.Contains(text, "✗") || !strings.Contains(text, "activate") {
		t.Fatalf("out %q", text)
	}
	if !strings.Contains(text, "all,beta") || strings.Contains(text, "alpha") {
		t.Fatalf("opt in hint %q", text)
	}
}
