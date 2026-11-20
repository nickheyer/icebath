package commands

import (
	"slices"
	"testing"
)

func TestSessionsParserMatchesArgparse(t *testing.T) {
	ns, err := bNamespace(t, "sessions prune", []string{"sessions", "prune"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"older_than", "newer_than", "before", "after", "source", "title", "end_reason", "cwd", "min_messages", "max_messages",
		"model", "provider", "user", "chat_id", "chat_type", "branch", "min_tokens", "max_tokens", "min_cost", "max_cost", "min_tool_calls", "max_tool_calls"} {
		if v, ok := ns[key]; !ok || v != nil {
			t.Fatalf("prune default %s = %v (present %v)", key, v, ok)
		}
	}
	for _, key := range []string{"dry_run", "yes", "include_archived", "include_pinned", "never_active", "force"} {
		if ns[key] != false {
			t.Fatalf("prune default %s = %v", key, ns[key])
		}
	}
	ns, err = bNamespace(t, "sessions archive", []string{"sessions", "archive", "--min-messages", "0", "--max-cost", "1.5", "--min-tool-calls", "3", "--older-than", "2d", "--include-open", "-y"})
	if err != nil || ns["min_messages"] != 0 || ns["max_cost"] != 1.5 || ns["min_tool_calls"] != 3 || ns["older_than"] != "2d" || ns["include_open"] != true || ns["yes"] != true || ns["max_messages"] != nil {
		t.Fatalf("archive %v %v", ns, err)
	}
	ns, err = bNamespace(t, "sessions repair-routing", []string{"sessions", "repair-routing"})
	if v, ok := ns["max_gap_seconds"]; err != nil || !ok || v != nil || ns["apply"] != false {
		t.Fatalf("repair-routing defaults %v %v", ns, err)
	}
	ns, err = bNamespace(t, "sessions repair-routing", []string{"sessions", "repair-routing", "--apply", "--max-gap-seconds", "30"})
	if err != nil || ns["max_gap_seconds"] != 30.0 || ns["apply"] != true {
		t.Fatalf("repair-routing %v %v", ns, err)
	}
	ns, err = bNamespace(t, "sessions import", []string{"sessions", "import", "--from", "codex", "/tmp/s.jsonl"})
	if _, raw := ns["from"]; err != nil || raw || ns["from_source"] != "codex" || ns["path"] != "/tmp/s.jsonl" {
		t.Fatalf("import %v %v", ns, err)
	}
	ns, err = bNamespace(t, "sessions import", []string{"sessions", "import"})
	if v, ok := ns["from_source"]; err != nil || !ok || v != nil || ns["path"] != nil {
		t.Fatalf("import defaults %v %v", ns, err)
	}
	if _, err = bNamespace(t, "sessions import", []string{"sessions", "import", "--from", "cursor"}); err == nil {
		t.Fatal("import accepted a source outside its choices")
	}
	ns, err = bNamespace(t, "sessions list", []string{"sessions", "list"})
	if err != nil || ns["limit"] != 20 || ns["source"] != nil || ns["workspace"] != nil || ns["command"] != "sessions" {
		t.Fatalf("list %v %v", ns, err)
	}
	ns, err = bNamespace(t, "sessions export", []string{"sessions", "export"})
	if err != nil || ns["format"] != "jsonl" || ns["lineage"] != "single" || ns["output"] != nil || ns["only"] != nil || ns["session_id"] != nil || ns["upload"] != false ||
		ns["public"] != false || ns["no_redact"] != false || ns["redact"] != false || ns["delete_after_verified"] != false || ns["force"] != false {
		t.Fatalf("export %v %v", ns, err)
	}
	if _, ok := ns["output"]; !ok {
		t.Fatalf("export without OUTPUT has no output dest %v", ns)
	}
	ns, err = bNamespace(t, "sessions export", []string{"sessions", "export", "out.jsonl", "--format", "md", "--session-id", "abc", "--only", "user-prompts"})
	if _, raw := ns["OUTPUT"]; err != nil || raw || ns["output"] != "out.jsonl" || ns["format"] != "md" || ns["session_id"] != "abc" || ns["only"] != "user-prompts" {
		t.Fatalf("export positional %v %v", ns, err)
	}
	if u := NewCLI().find("sessions export").Use; u != "export [OUTPUT]" {
		t.Fatalf("export usage %q", u)
	}
	ns, err = bNamespace(t, "sessions rename", []string{"sessions", "rename", "abc", "My", "new", "title"})
	if title, _ := ns["title"].([]string); err != nil || ns["session_id"] != "abc" || !slices.Equal(title, []string{"My", "new", "title"}) {
		t.Fatalf("rename %v %v", ns, err)
	}
	ns, err = bNamespace(t, "sessions recover", []string{"sessions", "recover", "--source", "a.db"})
	if err != nil || ns["source"] != "a.db" || ns["chunk_size"] != 1000 || ns["output"] != nil || ns["work_dir"] != nil || ns["report"] != nil || ns["inspect_only"] != false || ns["allow_partial"] != false {
		t.Fatalf("recover %v %v", ns, err)
	}
	if _, err = bNamespace(t, "sessions recover", []string{"sessions", "recover"}); err == nil {
		t.Fatal("recover ran without its required --source")
	}
	ns, err = bNamespace(t, "sessions refresh-prompts", []string{"sessions", "refresh-prompts", "--session", "abc", "--json"})
	if err != nil || ns["session"] != "abc" || ns["json"] != true {
		t.Fatalf("refresh-prompts %v %v", ns, err)
	}
	ns, err = bNamespace(t, "sessions repair-profiles", []string{"sessions", "repair-profiles"})
	if err != nil || ns["legacy_main"] != "report" || ns["apply"] != false || ns["json"] != false || ns["yes"] != false {
		t.Fatalf("repair-profiles %v %v", ns, err)
	}
	ns, err = bNamespace(t, "sessions retitle-skills", []string{"sessions", "retitle-skills"})
	if err != nil || ns["limit"] != 200 || ns["apply"] != false {
		t.Fatalf("retitle-skills %v %v", ns, err)
	}
	ns, err = bNamespace(t, "sessions browse", []string{"sessions", "browse"})
	if err != nil || ns["limit"] != 500 || ns["source"] != nil {
		t.Fatalf("browse %v %v", ns, err)
	}
	ns, err = bNamespace(t, "sessions pin", []string{"sessions", "pin", "a", "b"})
	if ids, _ := ns["session_ids"].([]string); err != nil || !slices.Equal(ids, []string{"a", "b"}) {
		t.Fatalf("pin %v %v", ns, err)
	}
	ns, err = bNamespace(t, "sessions set-journal-mode", []string{"sessions", "set-journal-mode", "wal"})
	if err != nil || ns["mode"] != "wal" || ns["db"] != nil || ns["force"] != false {
		t.Fatalf("set-journal-mode %v %v", ns, err)
	}
	if f := NewCLI().find("sessions recover").Flags().Lookup("allow-partial"); f == nil ||
		f.Usage != "Best-effort salvage across damaged row ranges; the output remains separate and every skipped range is recorded" {
		t.Fatalf("allow-partial flag %+v", f)
	}
}
