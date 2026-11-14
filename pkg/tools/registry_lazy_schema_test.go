package tools

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// A builder schema loads config at the first registry read only, once across concurrent readers
func TestSchemaBuilderReadsConfigOnlyOnFirstAccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	writeConfig := func(desc string) {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("lazy_probe:\n  description: "+desc+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("at registration")
	var reads atomic.Int32
	r := NewRegistry()
	r.Register(Def{Name: "lazy_probe", Toolset: "lazy", Handler: constHandler("{}"), SchemaBuilder: func() map[string]any {
		reads.Add(1)
		tree, err := config.LoadReadonly(context.Background(), dir, config.NewSecrets(dir).Lookup())
		if err != nil {
			panic(err)
		}
		section, _ := tree["lazy_probe"].(map[string]any)
		return map[string]any{"name": "lazy_probe", "description": section["description"],
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
	}})
	if n := reads.Load(); n != 0 {
		t.Fatalf("registration loaded config %d times", n)
	}
	if names := r.Names(); len(names) != 1 || names[0] != "lazy_probe" || reads.Load() != 0 {
		t.Fatalf("listing names %v loaded config %d times", names, reads.Load())
	}
	writeConfig("at first access")
	got := make([]*Def, 8)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = r.Get("lazy_probe")
		}()
	}
	wg.Wait()
	if n := reads.Load(); n != 1 {
		t.Fatalf("concurrent first reads loaded config %d times", n)
	}
	writeConfig("after first access")
	ctx := context.Background()
	entries := append(got, r.Get("lazy_probe"), r.Lookup(ctx, "", "lazy_probe"), r.Entry(ctx, &Env{}, "lazy_probe"),
		r.SnapshotRegistration("", "lazy_probe"), r.Snapshot(ctx, "")[0])
	for _, d := range entries {
		if d != got[0] || d.Schema["description"] != "at first access" || d.Description != "at first access" {
			t.Fatalf("entry %p schema %v description %q", d, d.Schema, d.Description)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("later reads loaded config again, %d loads", n)
	}
}

// Every reader path builds a deferred schema before handing the entry out
func TestSchemaBuilderRunsOnEveryReaderPath(t *testing.T) {
	ctx := context.Background()
	readers := map[string]func(r *Registry) *Def{
		"get":                   func(r *Registry) *Def { return r.Get("lazy_path") },
		"lookup":                func(r *Registry) *Def { return r.Lookup(ctx, "", "lazy_path") },
		"entry":                 func(r *Registry) *Def { return r.Entry(ctx, nil, "lazy_path") },
		"snapshot_registration": func(r *Registry) *Def { return r.SnapshotRegistration("", "lazy_path") },
		"snapshot":              func(r *Registry) *Def { return r.Snapshot(ctx, "")[0] },
	}
	for name, read := range readers {
		r := NewRegistry()
		builds := 0
		r.Register(Def{Name: "lazy_path", Toolset: "lazy", Handler: constHandler("{}"), SchemaBuilder: func() map[string]any {
			builds++
			return map[string]any{"name": "lazy_path", "description": "built " + name}
		}})
		d := read(r)
		if builds != 1 || d.Schema["description"] != "built "+name || d.Description != "built "+name {
			t.Fatalf("%s: builds %d schema %v description %q", name, builds, d.Schema, d.Description)
		}
	}
}

// A set Schema wins over the builder and an explicit description survives the build
func TestSchemaBuilderKeepsGivenSchemaAndDescription(t *testing.T) {
	r := NewRegistry()
	r.Register(Def{Name: "eager", Toolset: "lazy", Handler: constHandler("{}"), Schema: map[string]any{"name": "eager", "description": "given"},
		SchemaBuilder: func() map[string]any { panic("builder ran for a given schema") }})
	if d := r.Get("eager"); d.Schema["description"] != "given" || d.Description != "given" {
		t.Fatalf("eager entry %+v", d)
	}
	r.Register(Def{Name: "lazy_desc", Toolset: "lazy", Handler: constHandler("{}"), Description: "explicit",
		SchemaBuilder: func() map[string]any { return map[string]any{"name": "lazy_desc", "description": "from schema"} }})
	if d := r.Get("lazy_desc"); d.Schema["description"] != "from schema" || d.Description != "explicit" {
		t.Fatalf("lazy entry %+v", d)
	}
}

// A malformed built schema passes registration and panics at the first read like Register
func TestSchemaBuilderMalformedSchemaPanicsOnFirstRead(t *testing.T) {
	r := NewRegistry()
	r.Register(Def{Name: "lazy_bad", Toolset: "lazy", Handler: constHandler("{}"), SchemaBuilder: func() map[string]any { return nil }})
	defer func() {
		if rec := recover(); rec != "tools: Tool 'lazy_bad': schema must be a dict, got NoneType" {
			t.Fatalf("recovered %v", rec)
		}
	}()
	r.Get("lazy_bad")
	t.Fatal("malformed built schema did not panic")
}
