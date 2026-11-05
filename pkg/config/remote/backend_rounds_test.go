package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const legacySoul = "# Beta identity\n\n## Messaging other agents\nKEEP THIS PROFILE SECTION\n"

type migratorKey struct{}

func setProfile(p *stubPlane, name string, values map[string]any, version int, writer any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prof := p.profile(name)
	prof.values, prof.version, prof.writer = values, version, writer
}

func profileValues(p *stubPlane, name string) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return config.Clone(p.profile(name).values).(map[string]any)
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

// Another context's in memory migration paused after its steps leaves the edit diffing against the doc it read
func TestEditDuringAnInMemoryMigrationNeverSendsTheMigration(t *testing.T) {
	p := setupPlane(t)
	if config.LatestConfigVersion() <= 46 {
		t.Fatal("latest version too old")
	}
	setProfile(p, "default", map[string]any{"compression": map[string]any{"threshold_tokens": 256000}}, 1, 46)
	b := testBackend()
	if _, err := b.state(context.Background(), p.home); err != nil {
		t.Fatal(err)
	}
	stepsRan, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	real := runMigrations
	runMigrations = func(ctx context.Context, h string, current int) {
		real(ctx, h, current)
		if ctx.Value(migratorKey{}) != nil {
			once.Do(func() { close(stepsRan) })
			select {
			case <-resume:
			case <-time.After(10 * time.Second):
				t.Error("the edit waited for the migration")
			}
		}
	}
	t.Cleanup(func() { runMigrations = real })
	var failure error
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := context.WithValue(WithReadScope(context.Background()), migratorKey{}, true)
		_, failure = config.ReadConfigDoc(ctx, filepath.Join(p.home, "config.yaml"))
	}()
	waitOrFail(t, stepsRan)
	err := setValue(p, "display.personality", "pirate")
	close(resume)
	waitOrFail(t, done)
	if err != nil || failure != nil {
		t.Fatalf("edit %v migrator %v", err, failure)
	}
	patches := p.patches()
	if len(patches) != 1 {
		t.Fatalf("patches %d", len(patches))
	}
	raw, _ := json.Marshal(patches[0].body)
	if _, has := patches[0].body["unset"]; has || strings.Contains(string(raw), "compression") {
		t.Fatalf("migration sent %s", raw)
	}
	mustEqualJSON(t, "stored", profileValues(p, "default")["compression"], map[string]any{"threshold_tokens": 256000})
	later, err := config.ReadRawConfig(context.Background(), p.home)
	if err != nil || later["display"].(map[string]any)["personality"] != "pirate" {
		t.Fatalf("later %v %v", later, err)
	}
	if c, ok := later["compression"].(map[string]any); ok {
		if _, has := c["threshold_tokens"]; has {
			t.Fatal("not migrated in memory")
		}
	}
}

// A first read of another profile while the poll loop walks the roster leaves the only poller running
func TestProfileAddedWhileThePollerIteratesDoesNotKillIt(t *testing.T) {
	p := setupPlane(t)
	b := testBackend()
	ctx := context.Background()
	st, err := b.state(ctx, p.home)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(p.home, "profiles", "work")
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

// A value the old schema migration rewrites on every read is refused, a value it keeps is saved
func TestSettingAValueThatOldSchemaMigrationRewritesIsRefused(t *testing.T) {
	p := setupPlane(t)
	if config.LatestConfigVersion() <= 46 {
		t.Fatal("latest version too old")
	}
	ctx := context.Background()
	setProfile(p, "default", map[string]any{"display": map[string]any{"personality": "old"}}, 1, 46)
	if _, err := config.ReadRawConfig(ctx, p.home); err != nil {
		t.Fatal(err)
	}
	var notice bytes.Buffer
	prev := config.FacadeNoticeOut
	config.FacadeNoticeOut = &notice
	defer func() { config.FacadeNoticeOut = prev }()
	value := "256000"
	err := config.ConfigCommand(ctx, p.home, config.ConfigArgs{Command: "set", Key: "compression.threshold_tokens", Value: &value}, cmdSources())
	var exit *config.ConfigExit
	if !errors.As(err, &exit) || exit.Code != 1 || !strings.Contains(notice.String(), "compression.threshold_tokens") ||
		!strings.Contains(notice.String(), "schema v46") {
		t.Fatalf("set %v %q", err, notice.String())
	}
	if len(p.patches()) != 0 {
		t.Fatal("sent")
	}
	var valueErr *config.ConfigValueError
	if err := writeKey(p, "compression.threshold_tokens", 256000); !errors.As(err, &valueErr) || valueErr.Code != "config_migration_conflict" {
		t.Fatalf("write %v", err)
	}
	if err := writeKey(p, "compression.threshold_tokens", 300000); err != nil {
		t.Fatal(err)
	}
	mustEqualJSON(t, "stored", profileValues(p, "default")["compression"], map[string]any{"threshold_tokens": 300000})
	ResetForTests()
	doc, err := config.ReadRawConfig(ctx, p.home)
	if err != nil || doc["compression"].(map[string]any)["threshold_tokens"] != 300000 {
		t.Fatalf("read back %v %v", doc, err)
	}
}

// Another writer adds terminal.persistent between our read and our first PATCH, which then gets a 409
func absentSectionRace(t *testing.T, p *stubPlane) {
	t.Helper()
	setProfile(p, "default", map[string]any{"display": map[string]any{"personality": "concise"}}, 1, nil)
	if _, err := testBackend().ReadUserDoc(context.Background(), p.home); err != nil {
		t.Fatal(err)
	}
	original := request
	var raced atomic.Bool
	request = func(ctx context.Context, method, h, profile, etag string, body any, token string) (Response, error) {
		if method == "PATCH" && raced.CompareAndSwap(false, true) {
			p.mu.Lock()
			prof := p.profile("default")
			prof.values["terminal"] = map[string]any{"persistent": true}
			prof.version++
			p.mu.Unlock()
		}
		return original(ctx, method, h, profile, etag, body, token)
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
			patches := p.patches()
			if len(patches) != 2 {
				t.Fatalf("patches %d", len(patches))
			}
			retry := patches[1].body
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
	err := testBackend().WriteChanges(context.Background(), p.home, config.Changes{Set: []config.KeyValue{{Key: "terminal", Value: map[string]any{}}}})
	if err != nil {
		t.Fatal(err)
	}
	mustEqualJSON(t, "terminal", profileValues(p, "default")["terminal"], map[string]any{"persistent": true})
	if n := len(p.patches()); n != 1 {
		t.Fatalf("patches %d", n)
	}
}

// Unsetting a present section is still sent whole
func TestExplicitSectionReplacementStaysAReplacement(t *testing.T) {
	p := setupPlane(t)
	setProfile(p, "default", map[string]any{"terminal": map[string]any{"persistent": true, "timeout": 3}}, 1, nil)
	if err := testBackend().WriteChanges(context.Background(), p.home, config.Changes{Unset: []string{"terminal"}}); err != nil {
		t.Fatal(err)
	}
	patches := p.patches()
	mustEqualJSON(t, "unset", patches[len(patches)-1].body["unset"], []any{"terminal"})
	if _, has := profileValues(p, "default")["terminal"]; has {
		t.Fatal("terminal kept")
	}
}

// Reading a profile stamped 40 migrates it in memory without rewriting a sibling profile SOUL.md
func TestRemoteReadMigrationChangesNoLocalFile(t *testing.T) {
	p := setupPlane(t)
	soul := siblingWithLegacySoul(t, p.home)
	setProfile(p, "default", map[string]any{"display": map[string]any{"personality": "old"}}, 1, 40)
	doc, err := config.ReadConfigDoc(context.Background(), filepath.Join(p.home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := pyInt(doc.(map[string]any)["_config_version"]); v != config.LatestConfigVersion() {
		t.Fatalf("version %v", doc.(map[string]any)["_config_version"])
	}
	if mustRead(t, soul) != legacySoul || len(p.patches()) != 0 {
		t.Fatal("local file rewritten or sent")
	}
}

// The migration conflict check of a refused edit leaves every local file as it was
func TestRefusedWriteValidationChangesNoLocalFile(t *testing.T) {
	p := setupPlane(t)
	setProfile(p, "default", map[string]any{}, 1, 40)
	if _, err := config.ReadRawConfig(context.Background(), p.home); err != nil {
		t.Fatal(err)
	}
	soul := siblingWithLegacySoul(t, p.home)
	var valueErr *config.ConfigValueError
	if err := writeKey(p, "compression.threshold_tokens", 256000); !errors.As(err, &valueErr) || valueErr.Code != "config_migration_conflict" {
		t.Fatalf("write %v", err)
	}
	if mustRead(t, soul) != legacySoul || len(p.patches()) != 0 {
		t.Fatal("local file rewritten or sent")
	}
}

// Plane document change the poll lands, run once from the credential lookup of the next write
func pollBeforeWrite(t *testing.T, p *stubPlane, change func(prof *stubProfile)) {
	t.Helper()
	t.Setenv(idpEnvPrefix+"TOKEN_URL", "")
	var armed atomic.Bool
	b := testBackend()
	RegisterNousAccessToken(func(ctx context.Context, h string) (string, error) {
		if armed.CompareAndSwap(true, false) {
			p.mu.Lock()
			prof := p.profile("default")
			change(prof)
			prof.version++
			p.mu.Unlock()
			st, err := b.state(ctx, h)
			if err != nil || !b.pollOne(st) {
				t.Errorf("poll %v", err)
			}
		}
		return stubNousToken, nil
	})
	t.Cleanup(func() { RegisterNousAccessToken(nil) })
	if _, err := config.ReadRawConfig(context.Background(), p.home); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
}

// A poll between the read of config set and its save never turns the polled keys into edits
func TestPollBetweenReadAndSaveSendsOnlyTheCallersEdit(t *testing.T) {
	p := setupPlane(t)
	setProfile(p, "default", map[string]any{"display": map[string]any{"personality": "concise", "compact": false}}, 1, nil)
	pollBeforeWrite(t, p, func(prof *stubProfile) {
		prof.values["display"].(map[string]any)["compact"] = true
		prof.values["terminal"] = map[string]any{"timeout": 321}
	})
	if err := setValue(p, "display.personality", "pirate"); err != nil {
		t.Fatal(err)
	}
	patches := p.patches()
	if len(patches) != 1 {
		t.Fatalf("patches %v", patches)
	}
	if unset, _ := patches[0].body["unset"].([]any); len(unset) != 0 {
		t.Fatalf("unset %v", unset)
	}
	mustEqualJSON(t, "set", patches[0].body["set"], map[string]any{"display.personality": "pirate"})
	mustEqualJSON(t, "stored", profileValues(p, "default"), map[string]any{
		"display": map[string]any{"personality": "pirate", "compact": true}, "terminal": map[string]any{"timeout": 321}})
}

// The no omission guard read of an atomic config write never becomes the base of the stale caller doc
func TestPollBetweenReadAndAtomicConfigWriteSendsOnlyTheCallersEdit(t *testing.T) {
	p := setupPlane(t)
	setProfile(p, "default", map[string]any{"display": map[string]any{"personality": "concise", "compact": false}}, 1, nil)
	ctx := context.Background()
	path := filepath.Join(p.home, "config.yaml")
	raw, err := config.ReadUserConfigRaw(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	raw["display"].(map[string]any)["personality"] = "pirate"
	p.mu.Lock()
	prof := p.profile("default")
	prof.values["display"].(map[string]any)["compact"] = true
	prof.version++
	p.mu.Unlock()
	b := testBackend()
	st, err := b.state(ctx, p.home)
	if err != nil || !b.pollOne(st) {
		t.Fatalf("poll %v", err)
	}
	if err := config.AtomicConfigWrite(ctx, p.home, path, raw, ""); err != nil {
		t.Fatal(err)
	}
	patches := p.patches()
	if len(patches) != 1 {
		t.Fatalf("patches %v", patches)
	}
	mustEqualJSON(t, "set", patches[0].body["set"], map[string]any{"display.personality": "pirate"})
	mustEqualJSON(t, "display", profileValues(p, "default")["display"], map[string]any{"personality": "pirate", "compact": true})
}

func legacyProfile(t *testing.T, p *stubPlane) int {
	t.Helper()
	latest := config.LatestConfigVersion()
	if latest <= 46 {
		t.Fatal("latest version too old")
	}
	setProfile(p, "default", map[string]any{"compression": map[string]any{"threshold_tokens": 256000}}, 1, 46)
	if _, err := testBackend().state(context.Background(), p.home); err != nil {
		t.Fatal(err)
	}
	return latest
}

// Pauses the migrator context after its private migration ran its steps and before it publishes
func pauseFirstMigration(t *testing.T) (entered, resume chan struct{}) {
	entered, resume = make(chan struct{}), make(chan struct{})
	var once sync.Once
	real := runMigrations
	runMigrations = func(ctx context.Context, h string, current int) {
		real(ctx, h, current)
		if ctx.Value(migratorKey{}) != nil {
			once.Do(func() { close(entered) })
			select {
			case <-resume:
			case <-time.After(10 * time.Second):
				t.Error("the migrating context was never released")
			}
		}
	}
	t.Cleanup(func() { runMigrations = real })
	return entered, resume
}

// Runs read in its own goroutine and read scope marked as the migrator
func startMigrator(read func(ctx context.Context) (any, error)) (done chan struct{}, result *any, failure *error) {
	done, result, failure = make(chan struct{}), new(any), new(error)
	go func() {
		defer close(done)
		*result, *failure = read(context.WithValue(WithReadScope(context.Background()), migratorKey{}, true))
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

// A second reader while the first migration is paused, unpublished, still gets migrated output
func TestConcurrentReaderNeverSeesTheUnmigratedDoc(t *testing.T) {
	p := setupPlane(t)
	latest := legacyProfile(t, p)
	entered, resume := pauseFirstMigration(t)
	cfg := filepath.Join(p.home, "config.yaml")
	done, first, failure := startMigrator(func(ctx context.Context) (any, error) { return config.ReadConfigDoc(ctx, cfg) })
	waitOrFail(t, entered)
	second, err := config.ReadConfigDoc(context.Background(), cfg)
	close(resume)
	waitOrFail(t, done)
	if err != nil || *failure != nil {
		t.Fatalf("read %v migrator %v", err, *failure)
	}
	assertMigrated(t, second, latest)
	assertMigrated(t, *first, latest)
	if len(p.patches()) != 0 {
		t.Fatal("sent")
	}
}

// A readonly reader during a paused migration also gets only migrated output
func TestConcurrentReadonlyReaderNeverSeesTheUnmigratedDoc(t *testing.T) {
	p := setupPlane(t)
	latest := legacyProfile(t, p)
	entered, resume := pauseFirstMigration(t)
	cfg := filepath.Join(p.home, "config.yaml")
	done, _, failure := startMigrator(func(ctx context.Context) (any, error) { return config.ReadConfigDocReadonly(ctx, cfg) })
	waitOrFail(t, entered)
	doc, err := config.ReadConfigDocReadonly(context.Background(), cfg)
	close(resume)
	waitOrFail(t, done)
	if err != nil || *failure != nil {
		t.Fatalf("read %v migrator %v", err, *failure)
	}
	assertMigrated(t, doc, latest)
}

// The edit diffs against completed migration output, the migration removal never sent and the old stamp kept
func TestWriterDuringAMigrationSendsOnlyItsEdit(t *testing.T) {
	p := setupPlane(t)
	legacyProfile(t, p)
	entered, resume := pauseFirstMigration(t)
	done, _, failure := startMigrator(func(ctx context.Context) (any, error) {
		return config.ReadConfigDoc(ctx, filepath.Join(p.home, "config.yaml"))
	})
	waitOrFail(t, entered)
	err := setValue(p, "display.personality", "pirate")
	close(resume)
	waitOrFail(t, done)
	if err != nil || *failure != nil {
		t.Fatalf("edit %v migrator %v", err, *failure)
	}
	patches := p.patches()
	if len(patches) != 1 {
		t.Fatalf("patches %d", len(patches))
	}
	body := patches[0].body
	raw, _ := json.Marshal(body)
	_, unset := body["unset"]
	_, stamp := body["writerConfigVersion"]
	if unset || stamp || strings.Contains(string(raw), "compression") {
		t.Fatalf("body %s", raw)
	}
	mustEqualJSON(t, "stored", profileValues(p, "default")["compression"], map[string]any{"threshold_tokens": 256000})
	later, err := config.ReadRawConfig(context.Background(), p.home)
	if err != nil || later["display"].(map[string]any)["personality"] != "pirate" {
		t.Fatalf("later %v %v", later, err)
	}
	assertMigrated(t, later, config.LatestConfigVersion())
}

// A poll installing a newer legacy doc after the reader completed the old one sends the reader round again
func TestPollInstallingANewerDocMidReadIsMigratedBeforeItIsReturned(t *testing.T) {
	p := setupPlane(t)
	latest := legacyProfile(t, p)
	b := testBackend()
	st, _ := b.state(context.Background(), p.home)
	real := complete
	var polled atomic.Bool
	complete = func(b *remoteBackend, ctx context.Context, state *profileState) error {
		err := real(b, ctx, state)
		if polled.CompareAndSwap(false, true) {
			setProfile(p, "default", map[string]any{"compression": map[string]any{"threshold_tokens": 256000},
				"display": map[string]any{"personality": "pirate"}}, 2, 46)
			if !b.pollOne(st) {
				t.Error("poll installed nothing")
			}
			st.mu.Lock()
			pending := !st.postprocessed
			st.mu.Unlock()
			if !pending {
				t.Error("poll left the doc postprocessed")
			}
		}
		return err
	}
	t.Cleanup(func() { complete = real })
	doc, err := config.ReadConfigDoc(context.Background(), filepath.Join(p.home, "config.yaml"))
	if err != nil || !polled.Load() {
		t.Fatalf("read %v polled %v", err, polled.Load())
	}
	assertMigrated(t, doc, latest)
	if doc.(map[string]any)["display"].(map[string]any)["personality"] != "pirate" {
		t.Fatal("the first doc was returned")
	}
}

// A migration step reads its private copy without starting another pass, its persist staying in memory
func TestMigrationRecursionStillSeesItsPrivateCopy(t *testing.T) {
	p := setupPlane(t)
	latest := config.LatestConfigVersion()
	setProfile(p, "default", map[string]any{"old_key": 1}, 1, latest-1)
	var seen []any
	real := runMigrations
	runMigrations = func(ctx context.Context, h string, current int) {
		doc, err := config.ReadRawConfig(ctx, h)
		if err != nil {
			t.Error(err)
			return
		}
		seen = append(seen, doc["_config_version"])
		delete(doc, "old_key")
		doc["new_key"] = 2
		backend, err := config.GetConfigBackend(ctx)
		if err == nil {
			err = backend.ApplyInMemory(ctx, h, doc)
		}
		if err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { runMigrations = real })
	doc, err := config.ReadRawConfig(context.Background(), p.home)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != latest-1 {
		t.Fatalf("seen %v", seen)
	}
	if _, has := doc["old_key"]; has || doc["new_key"] != 2 || len(p.patches()) != 0 {
		t.Fatalf("doc %v", doc)
	}
}
