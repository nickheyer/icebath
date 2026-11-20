package state

import (
	"context"
	"testing"
)

func modelConfigDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenHome(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestModelConfigValueRoundTrip(t *testing.T) {
	db := modelConfigDB(t)
	if err := db.CreateSession(NewSession{ID: "s1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.GetModelConfigValue("s1", "onboarding_run"); ok {
		t.Fatal("absent key")
	}
	if _, ok, err := db.GetModelConfigValue("missing", "k"); ok || err != nil {
		t.Fatal("missing session")
	}
	tip, err := db.UpdateConversationModelConfigValue("s1", "onboarding_run", func(current any) any {
		if current != nil {
			t.Fatal("fresh key")
		}
		return map[string]any{"phase": "onboarding"}
	})
	if err != nil || tip != "s1" {
		t.Fatal(tip, err)
	}
	v, ok, _ := db.GetModelConfigValue("s1", "onboarding_run")
	if !ok || v.(map[string]any)["phase"] != "onboarding" {
		t.Fatal(v)
	}
	ids, _ := db.SessionIDsWithModelConfigKey("onboarding_run")
	if len(ids) != 1 || ids[0] != "s1" {
		t.Fatal(ids)
	}
	if _, err := db.UpdateConversationModelConfigValue("s1", "onboarding_run", func(any) any { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.GetModelConfigValue("s1", "onboarding_run"); ok {
		t.Fatal("nil must remove the key")
	}
	if tip, _ := db.UpdateConversationModelConfigValue("ghost", "k", func(any) any { return 1 }); tip != "" {
		t.Fatal("missing row writes nothing")
	}
}

func TestResolveResumeSessionIDFollowsCompressionAndContinuations(t *testing.T) {
	db := modelConfigDB(t)
	for _, id := range []string{"root", "child", "cont", "branch"} {
		parent := ""
		switch id {
		case "child":
			parent = "root"
		case "cont", "branch":
			parent = "child"
		}
		n := NewSession{ID: id, Source: "cli", ParentSessionID: parent}
		if id == "branch" {
			n.ModelConfig = map[string]any{"_branched_from": "child"}
		}
		if err := db.CreateSession(n); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.EndSession("root", "compression"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ResolveResumeSessionID("root"); got != "child" {
		t.Fatalf("tip without messages %q", got)
	}
	if _, err := db.AppendMessage(context.Background(), "cont", map[string]any{"role": "user", "content": "hi"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ResolveResumeSessionID("root"); got != "cont" {
		t.Fatalf("deepest continuation with messages %q", got)
	}
	if got, err := db.ResolveResumeSessionID(""); err != nil || got != "" {
		t.Fatal("empty id")
	}
}
