package acp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

func provenanceDB(t *testing.T) *state.SessionDB {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db, err := state.NewSessionDB(context.Background(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mkSession(t *testing.T, db *state.SessionDB, sid, parent string) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "acp", ParentSessionID: parent}); err != nil {
		t.Fatal(err)
	}
}

func provGet(t *testing.T, p interface{ Get(string) (any, bool) }, key string) any {
	t.Helper()
	v, _ := p.Get(key)
	return v
}

func TestRootSessionNoCompression(t *testing.T) {
	db := provenanceDB(t)
	mkSession(t, db, "root1", "")
	prov := BuildSessionProvenance(context.Background(), db, "acp-1", "root1", "")
	if provGet(t, prov, "acpSessionId") != "acp-1" || provGet(t, prov, "currentHermesSessionId") != "root1" ||
		provGet(t, prov, "rootHermesSessionId") != "root1" || provGet(t, prov, "parentHermesSessionId") != nil ||
		provGet(t, prov, "sessionKind") != "root" || provGet(t, prov, "compressionDepth") != int64(0) {
		t.Fatalf("prov %v", prov)
	}
	if _, ok := prov.Get("reason"); ok {
		t.Fatal("reason present")
	}
}

func TestCompressionSplitContinuation(t *testing.T) {
	db := provenanceDB(t)
	mkSession(t, db, "old", "")
	if err := db.EndSession("old", "compression"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	mkSession(t, db, "new", "old")
	prov := BuildSessionProvenance(context.Background(), db, "acp-1", "new", "old")
	if provGet(t, prov, "sessionKind") != "continuation" || provGet(t, prov, "parentHermesSessionId") != "old" ||
		provGet(t, prov, "rootHermesSessionId") != "old" || provGet(t, prov, "compressionDepth") != int64(1) ||
		provGet(t, prov, "previousHermesSessionId") != "old" || provGet(t, prov, "reason") != "compression" ||
		provGet(t, prov, "creatorKind") != "compression" {
		t.Fatalf("prov %v", prov)
	}
}

func TestNonCompressionParentIsRootNotContinuation(t *testing.T) {
	db := provenanceDB(t)
	mkSession(t, db, "p", "")
	mkSession(t, db, "c", "p")
	prov := BuildSessionProvenance(context.Background(), db, "acp-1", "c", "")
	if provGet(t, prov, "sessionKind") != "root" || provGet(t, prov, "compressionDepth") != int64(0) ||
		provGet(t, prov, "rootHermesSessionId") != "p" {
		t.Fatalf("prov %v", prov)
	}
}

func TestMetaWrapperShape(t *testing.T) {
	db := provenanceDB(t)
	mkSession(t, db, "root1", "")
	meta := SessionProvenanceMeta(context.Background(), db, "acp-1", "root1", "")
	if meta.Len() != 1 || meta.Keys[0] != "hermes" {
		t.Fatalf("meta %v", meta)
	}
	inner := provGet(t, meta, "hermes").(interface{ Get(string) (any, bool) })
	sp, ok := inner.Get("sessionProvenance")
	if !ok || provGet(t, sp.(interface{ Get(string) (any, bool) }), "currentHermesSessionId") != "root1" {
		t.Fatalf("meta %v", meta)
	}
}
