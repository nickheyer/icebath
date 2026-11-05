package remote

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Pauses the first GET after its response arrived until release closes, later requests passing through
func holdGets(t *testing.T) (fetched, release chan struct{}) {
	fetched, release = make(chan struct{}), make(chan struct{})
	original := request
	var once sync.Once
	request = func(ctx context.Context, method, h, profile, etag string, body any, token string) (Response, error) {
		resp, err := original(ctx, method, h, profile, etag, body, token)
		if method == "GET" {
			first := false
			once.Do(func() { first = true })
			if first {
				close(fetched)
				select {
				case <-release:
				case <-time.After(10 * time.Second):
					t.Error("release never came")
				}
			}
		}
		return resp, err
	}
	t.Cleanup(func() { request = original })
	return fetched, release
}

func waitOrFail(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}
}

func stateDoc(st *profileState, key string) (int, any) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.profileVersion, st.doc["display"].(map[string]any)[key]
}

func TestPollInFlightDoesNotOverwriteAnAcknowledgedWrite(t *testing.T) {
	p := setupPlane(t)
	p.upper = map[string]any{"display": map[string]any{"personality": "old"}}
	b := testBackend()
	st, err := b.state(context.Background(), p.home)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.upper["display"].(map[string]any)["personality"] = "upper-change"
	p.mu.Unlock()
	fetched, release := holdGets(t)
	done := make(chan struct{})
	go func() { b.pollOne(st); close(done) }()
	waitOrFail(t, fetched)
	if err := writeKey(p, "display.personality", "my-write"); err != nil {
		t.Fatal(err)
	}
	if v, d := stateDoc(st, "personality"); v != 1 || d != "my-write" {
		t.Fatalf("after write %d %v", v, d)
	}
	close(release)
	waitOrFail(t, done)
	if v, d := stateDoc(st, "personality"); v != 1 || d != "my-write" {
		t.Fatalf("after poll %d %v", v, d)
	}
	mustEqualJSON(t, "values", p.profile("default").values, map[string]any{"display": map[string]any{"personality": "my-write"}})
}

func TestStaleFetchWithSameProfileVersionIsDropped(t *testing.T) {
	p := setupPlane(t)
	p.upper = map[string]any{"display": map[string]any{"personality": "old"}}
	b := testBackend()
	st, err := b.state(context.Background(), p.home)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.upper["display"].(map[string]any)["personality"] = "v1"
	p.mu.Unlock()
	fetched, release := holdGets(t)
	done := make(chan struct{})
	go func() { b.pollOne(st); close(done) }()
	waitOrFail(t, fetched)
	p.mu.Lock()
	p.upper["display"].(map[string]any)["personality"] = "v2"
	p.mu.Unlock()
	if !b.pollOne(st) {
		t.Fatal("newer fetch not installed")
	}
	if _, d := stateDoc(st, "personality"); d != "v2" {
		t.Fatalf("doc %v", d)
	}
	close(release)
	waitOrFail(t, done)
	if v, d := stateDoc(st, "personality"); v != 0 || d != "v2" {
		t.Fatalf("stale fetch installed %d %v", v, d)
	}
}

func oldCompressionProfile(t *testing.T, p *stubPlane) {
	t.Helper()
	if config.LatestConfigVersion() <= 46 {
		t.Fatal("latest version too old")
	}
	p.upper = map[string]any{"compression": map[string]any{"threshold_tokens": 256000}}
	p.profile("default").writer = 46
	doc, err := config.ReadRawConfig(context.Background(), p.home)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := doc["compression"].(map[string]any); ok {
		if _, has := c["threshold_tokens"]; has {
			t.Fatal("precondition: migrated in memory")
		}
	}
}

func onlyPirate(t *testing.T, body map[string]any) {
	t.Helper()
	mustEqualJSON(t, "set", body["set"], map[string]any{"display": map[string]any{"personality": "pirate"}})
	if _, has := body["unset"]; has {
		t.Fatalf("migration sent %v", body["unset"])
	}
}

func TestMigrationIsNotSentByAnUnrelatedWrite(t *testing.T) {
	p := setupPlane(t)
	oldCompressionProfile(t, p)
	if err := setValue(p, "display.personality", "pirate"); err != nil {
		t.Fatal(err)
	}
	patches := p.patches()
	if len(patches) != 1 {
		t.Fatalf("patches %d", len(patches))
	}
	onlyPirate(t, patches[0].body)
}

func TestMigrationIsNotSentAfterACASReread(t *testing.T) {
	p := setupPlane(t)
	oldCompressionProfile(t, p)
	p.mu.Lock()
	p.profile("default").version = 3
	p.mu.Unlock()
	if err := setValue(p, "display.personality", "pirate"); err != nil {
		t.Fatal(err)
	}
	patches := p.patches()
	if len(patches) != 2 || patches[0].body["expectedVersion"] != 0 || patches[1].body["expectedVersion"] != 3 {
		t.Fatalf("patches %v", patches)
	}
	for _, pt := range patches {
		onlyPirate(t, pt.body)
	}
}

func TestBulkSaveAfterMigrationSendsOnlyTheEdit(t *testing.T) {
	p := setupPlane(t)
	oldCompressionProfile(t, p)
	ctx := context.Background()
	doc, _ := config.ReadRawConfig(ctx, p.home)
	display, _ := doc["display"].(map[string]any)
	if display == nil {
		display = map[string]any{}
		doc["display"] = display
	}
	display["personality"] = "pirate"
	if err := config.SaveConfig(ctx, p.home, doc, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	patches := p.patches()
	if len(patches) != 1 {
		t.Fatalf("patches %d", len(patches))
	}
	onlyPirate(t, patches[0].body)
	if _, has := patches[0].body["set"].(map[string]any)["compression"]; has {
		t.Fatal("compression sent")
	}
}

func TestUnrelatedWriteKeepsAnOldProfileStampSoLaterReadsStillMigrate(t *testing.T) {
	p := setupPlane(t)
	prof := p.profile("default")
	prof.values, prof.writer = map[string]any{"compression": map[string]any{"threshold_tokens": 256000}}, 46
	ctx := context.Background()
	noThreshold := func(doc map[string]any) bool {
		c, _ := doc["compression"].(map[string]any)
		_, has := c["threshold_tokens"]
		return !has
	}
	if doc, _ := config.ReadRawConfig(ctx, p.home); !noThreshold(doc) {
		t.Fatal("precondition")
	}
	if err := setValue(p, "display.personality", "pirate"); err != nil {
		t.Fatal(err)
	}
	patches := p.patches()
	if _, has := patches[0].body["writerConfigVersion"]; len(patches) != 1 || has || prof.writer != 46 {
		t.Fatalf("stamp sent %v", patches[0].body)
	}
	mustEqualJSON(t, "stored", prof.values["compression"], map[string]any{"threshold_tokens": 256000})
	ResetForTests()
	doc, err := config.ReadRawConfig(ctx, p.home)
	if err != nil || !noThreshold(doc) || doc["display"].(map[string]any)["personality"] != "pirate" || len(p.patches()) != 1 {
		t.Fatalf("later read %v %v", doc, err)
	}
}

func TestWriteStampsACurrentOrUnstampedProfileLevel(t *testing.T) {
	for _, stored := range []any{config.LatestConfigVersion(), nil} {
		p := setupPlane(t)
		p.profile("default").writer = stored
		if err := setValue(p, "display.personality", "pirate"); err != nil {
			t.Fatal(err)
		}
		patches := p.patches()
		if len(patches) != 1 || patches[0].body["writerConfigVersion"] != config.LatestConfigVersion() || p.profile("default").writer != config.LatestConfigVersion() {
			t.Fatalf("stored %v patches %v", stored, patches)
		}
	}
}

func TestWriteNeverLowersANewerProfileStamp(t *testing.T) {
	p := setupPlane(t)
	newer := config.LatestConfigVersion() + 1
	p.profile("default").writer = newer
	if err := setValue(p, "display.personality", "pirate"); err != nil {
		t.Fatal(err)
	}
	patches := p.patches()
	if _, has := patches[0].body["writerConfigVersion"]; len(patches) != 1 || has || p.profile("default").writer != newer {
		t.Fatalf("stamp lowered %v", patches)
	}
	if !strings.Contains(testBackend().Describe(context.Background(), p.home), "profile level v1") {
		t.Fatal("describe")
	}
}
