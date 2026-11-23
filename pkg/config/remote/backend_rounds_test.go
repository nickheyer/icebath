package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/testfakes/configplane"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const legacySoul = "# Beta identity\n\n## Messaging other agents\nKEEP THIS PROFILE SECTION\n"

type migratorKey struct{}

func setProfile(p *configplane.Plane, name string, values map[string]any, version int, writer any) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	prof := p.Profile(name)
	prof.Values, prof.Version, prof.Writer = values, version, writer
}

func profileValues(p *configplane.Plane, name string) map[string]any {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	return config.Clone(p.Profile(name).Values).(map[string]any)
}

func siblingWithLegacySoul(t *testing.T, h string) string {
	t.Helper()
	beta := filepath.Join(h, "profiles", "beta")
	if err := os.MkdirAll(beta, 0o755); err != nil {
		t.Fatal(err)
	}
	soul := filepath.Join(beta, "SOUL.md")
	if err := os.WriteFile(soul, []byte(legacySoul), 0o644); err != nil {
		t.Fatal(err)
	}
	return soul
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A first read of another profile while the poll loop walks the roster leaves the only poller running
func TestProfileAddedWhileThePollerIteratesDoesNotKillIt(t *testing.T) {
	p := setupPlane(t)
	b := testBackend()
	ctx := context.Background()
	st, err := b.state(ctx, p.Home)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(p.Home, "profiles", "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	fetched, release := holdGets(t)
	st.mu.Lock()
	st.nextPoll = time.Time{}
	st.mu.Unlock()
	done := make(chan struct{})
	go b.pollLoop(done)
	waitOrFail(t, fetched)
	_, err = b.state(ctx, work)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("the poll loop died")
	case <-time.After(2 * time.Second):
	}
	b.mu.Lock()
	_, inserted := b.states[home.Key(work)]
	b.mu.Unlock()
	if !inserted {
		t.Fatal("work profile state missing")
	}
}

// Another writer adds terminal.persistent between our read and our first PATCH, which then gets a 409
func absentSectionRace(t *testing.T, p *configplane.Plane) {
	t.Helper()
	setProfile(p, "default", map[string]any{"display": map[string]any{"personality": "concise"}}, 1, nil)
	if _, err := testBackend().ReadUserDoc(context.Background(), p.Home); err != nil {
		t.Fatal(err)
	}
	original := request
	var raced atomic.Bool
	request = func(ctx context.Context, method, profile, etag string, body any, token string) (Response, error) {
		if method == "PATCH" && raced.CompareAndSwap(false, true) {
			p.Mu.Lock()
			prof := p.Profile("default")
			prof.Values["terminal"] = map[string]any{"persistent": true}
			prof.Version++
			p.Mu.Unlock()
		}
		return original(ctx, method, profile, etag, body, token)
	}
	t.Cleanup(func() { request = original })
}

// The 409 retry adds the one key to the reread doc instead of replacing the section another writer created
func TestCASRetryKeepsASiblingAddedToASectionTheDocLacked(t *testing.T) {
	for _, entry := range []string{"set_config_value", "write_config_key"} {
		t.Run(entry, func(t *testing.T) {
			p := setupPlane(t)
			absentSectionRace(t, p)
			var err error
			if entry == "set_config_value" {
				err = setValue(p, "terminal.timeout", "321")
			} else {
				err = writeKey(p, "terminal.timeout", 321)
			}
			if err != nil {
				t.Fatal(err)
			}
			mustEqualJSON(t, "terminal", profileValues(p, "default")["terminal"], map[string]any{"persistent": true, "timeout": 321})
			patches := p.Patches()
			if len(patches) != 2 {
				t.Fatalf("patches %d", len(patches))
			}
			retry := patches[1].Body
			if _, has := retry["unset"]; has || retry["expectedVersion"] != 2 {
				t.Fatalf("retry %v", retry)
			}
			mustEqualJSON(t, "set", retry["set"], map[string]any{"terminal.timeout": 321})
		})
	}
}

// An empty mapping the edit adds never replaces the mapping with keys another writer created meanwhile
func TestCASRetryOfANewEmptySectionKeepsAConcurrentOne(t *testing.T) {
	p := setupPlane(t)
	absentSectionRace(t, p)
	err := testBackend().WriteChanges(context.Background(), p.Home, config.Changes{Set: []config.KeyValue{{Key: "terminal", Value: map[string]any{}}}})
	if err != nil {
		t.Fatal(err)
	}
	mustEqualJSON(t, "terminal", profileValues(p, "default")["terminal"], map[string]any{"persistent": true})
	if n := len(p.Patches()); n != 1 {
		t.Fatalf("patches %d", n)
	}
}

// Unsetting a present section is still sent whole
func TestExplicitSectionReplacementStaysAReplacement(t *testing.T) {
	p := setupPlane(t)
	setProfile(p, "default", map[string]any{"terminal": map[string]any{"persistent": true, "timeout": 3}}, 1, nil)
	if err := testBackend().WriteChanges(context.Background(), p.Home, config.Changes{Unset: []string{"terminal"}}); err != nil {
		t.Fatal(err)
	}
	patches := p.Patches()
	mustEqualJSON(t, "unset", patches[len(patches)-1].Body["unset"], []any{"terminal"})
	if _, has := profileValues(p, "default")["terminal"]; has {
		t.Fatal("terminal kept")
	}
}

// The migration conflict check of a refused edit leaves every local file as it was
func TestRefusedWriteValidationChangesNoLocalFile(t *testing.T) {
	p := setupPlane(t)
	setProfile(p, "default", map[string]any{}, 1, 40)
	if _, err := config.ReadRawConfig(context.Background(), p.Home); err != nil {
		t.Fatal(err)
	}
	soul := siblingWithLegacySoul(t, p.Home)
	var valueErr *config.ConfigValueError
	if err := writeKey(p, "compression.threshold_tokens", 256000); !errors.As(err, &valueErr) || valueErr.Code != "config_migration_conflict" {
		t.Fatalf("write %v", err)
	}
	if mustRead(t, soul) != legacySoul || len(p.Patches()) != 0 {
		t.Fatal("local file rewritten or sent")
	}
}

// Plane document change the poll lands, run once from the credential lookup of the next write
func pollBeforeWrite(t *testing.T, p *configplane.Plane, change func(prof *configplane.Profile)) {
	t.Helper()
	var armed atomic.Bool
	b := testBackend()
	originalToken := planeToken
	planeToken = func(ctx context.Context) (string, error) {
		if armed.CompareAndSwap(true, false) {
			p.Mu.Lock()
			prof := p.Profile("default")
			change(prof)
			prof.Version++
			p.Mu.Unlock()
			st, err := b.state(ctx, p.Home)
			if err != nil || !b.pollOne(st) {
				t.Errorf("poll %v", err)
			}
		}
		return configplane.Token, nil
	}
	t.Cleanup(func() { planeToken = originalToken })
	if _, err := config.ReadRawConfig(context.Background(), p.Home); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
}

// A poll between the read of config set and its save never turns the polled keys into edits
func TestPollBetweenReadAndSaveSendsOnlyTheCallersEdit(t *testing.T) {
	p := setupPlane(t)
	setProfile(p, "default", map[string]any{"display": map[string]any{"personality": "concise", "compact": false}}, 1, nil)
	pollBeforeWrite(t, p, func(prof *configplane.Profile) {
		prof.Values["display"].(map[string]any)["compact"] = true
		prof.Values["terminal"] = map[string]any{"timeout": 321}
	})
	if err := setValue(p, "display.personality", "pirate"); err != nil {
		t.Fatal(err)
	}
	patches := p.Patches()
	if len(patches) != 1 {
		t.Fatalf("patches %v", patches)
	}
	if unset, _ := patches[0].Body["unset"].([]any); len(unset) != 0 {
		t.Fatalf("unset %v", unset)
	}
	mustEqualJSON(t, "set", patches[0].Body["set"], map[string]any{"display.personality": "pirate"})
	mustEqualJSON(t, "stored", profileValues(p, "default"), map[string]any{
		"display": map[string]any{"personality": "pirate", "compact": true}, "terminal": map[string]any{"timeout": 321}})
}

// The no omission guard read of an atomic config write never becomes the base of the stale caller doc
func TestPollBetweenReadAndAtomicConfigWriteSendsOnlyTheCallersEdit(t *testing.T) {
	p := setupPlane(t)
	setProfile(p, "default", map[string]any{"display": map[string]any{"personality": "concise", "compact": false}}, 1, nil)
	ctx := context.Background()
	path := filepath.Join(p.Home, "config.yaml")
	raw, err := config.ReadUserConfigRaw(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	raw["display"].(map[string]any)["personality"] = "pirate"
	p.Mu.Lock()
	prof := p.Profile("default")
	prof.Values["display"].(map[string]any)["compact"] = true
	prof.Version++
	p.Mu.Unlock()
	b := testBackend()
	st, err := b.state(ctx, p.Home)
	if err != nil || !b.pollOne(st) {
		t.Fatalf("poll %v", err)
	}
	if err := config.AtomicConfigWrite(ctx, p.Home, path, raw, ""); err != nil {
		t.Fatal(err)
	}
	patches := p.Patches()
	if len(patches) != 1 {
		t.Fatalf("patches %v", patches)
	}
	mustEqualJSON(t, "set", patches[0].Body["set"], map[string]any{"display.personality": "pirate"})
	mustEqualJSON(t, "display", profileValues(p, "default")["display"], map[string]any{"personality": "pirate", "compact": true})
}

// Runs read in its own goroutine marked as the migrator
func startMigrator(read func(ctx context.Context) (any, error)) (done chan struct{}, result *any, failure *error) {
	done, result, failure = make(chan struct{}), new(any), new(error)
	go func() {
		defer close(done)
		*result, *failure = read(context.WithValue(context.Background(), migratorKey{}, true))
	}()
	return done, result, failure
}

func assertMigrated(t *testing.T, doc any, latest int) {
	t.Helper()
	m := doc.(map[string]any)
	if v, _ := pyInt(m["_config_version"]); v != latest {
		t.Fatalf("version %v", m["_config_version"])
	}
	if c, ok := m["compression"].(map[string]any); ok {
		if _, has := c["threshold_tokens"]; has {
			t.Fatal("unmigrated doc returned")
		}
	}
}
