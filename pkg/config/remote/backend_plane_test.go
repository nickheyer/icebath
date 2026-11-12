package remote

import (
	"context"
	"errors"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestMigrationOfAReplacedDocDoesNotTouchTheReplacement(t *testing.T) {
	p := setupPlane(t)
	latest := config.LatestConfigVersion()
	prof := p.Profile("default")
	prof.Values, prof.Writer = map[string]any{"compression": map[string]any{"threshold_tokens": 256000}}, 46
	b := testBackend()
	st, err := b.state(context.Background(), p.Home)
	if err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	saved := runMigrations
	first := true
	runMigrations = func(ctx context.Context, h string, current int) {
		if first {
			first = false
			close(entered)
			<-resume
		}
		saved(ctx, h, current)
	}
	t.Cleanup(func() { runMigrations = saved })
	readErr := make(chan error, 1)
	go func() {
		_, err := config.ReadRawConfig(context.Background(), p.Home)
		readErr <- err
	}()
	waitOrFail(t, entered)
	p.Mu.Lock()
	prof.Writer, prof.Version = latest, 1
	p.Mu.Unlock()
	if !b.pollOne(st) {
		t.Fatal("poll did not install")
	}
	st.mu.Lock()
	mustEqualJSON(t, "replacement", st.doc["compression"], map[string]any{"threshold_tokens": 256000})
	st.mu.Unlock()
	close(resume)
	select {
	case err := <-readErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reader stuck")
	}
	doc, err := config.ReadRawConfig(context.Background(), p.Home)
	if err != nil || doc["_config_version"] != latest || len(p.Patches()) != 0 {
		t.Fatalf("doc %v %v", doc, err)
	}
	mustEqualJSON(t, "kept", doc["compression"], map[string]any{"threshold_tokens": 256000})
}

func TestPlaneRefusalsKeepThePlaneCode(t *testing.T) {
	cases := []struct {
		status int
		code   string
		value  bool
	}{
		{400, "config_value_invalid", true}, {400, "config_path_invalid", true}, {400, "config_path_reserved", true},
		{400, "config_secret_literal", true}, {413, "config_level_too_large", true}, {413, "config_body_too_large", true},
		{400, "config_request_invalid", false}, {500, "internal", false},
	}
	for _, c := range cases {
		err := writeError(Response{Status: c.status, Body: map[string]any{"error": c.code, "message": "refused", "path": "a.b"}})
		var verr *config.ConfigValueError
		var werr *config.ConfigWriteError
		if errors.As(err, &verr) != c.value || !errors.As(err, &werr) || werr.Code != c.code {
			t.Fatalf("%d %s -> %v", c.status, c.code, err)
		}
		if c.value && err.Error() != "Remote Config refused the write: refused (path a.b)" {
			t.Fatalf("detail %q", err.Error())
		}
	}
}

func multiGroupPlane(p *configplane.Plane) {
	p.Upper = map[string]any{"terminal": map[string]any{"backend": "docker"}, "model": map[string]any{"default": "hermes-4"}}
	p.UpperLocks = []map[string]any{{"path": "terminal.backend", "level": "group", "groupId": "g-sec"}}
	p.Groups = []map[string]any{{"groupId": "g-sec", "priority": 10, "version": 3}, {"groupId": "g-eng", "priority": 5, "version": 1}}
	p.GroupProvenance = map[string]any{"terminal": map[string]any{"backend": "g-sec"}}
}

func TestMultiGroupResponseFieldsAreIgnoredAndGroupLockRefusesClientSide(t *testing.T) {
	p := setupPlane(t)
	multiGroupPlane(p)
	ctx := context.Background()
	doc, err := config.ReadRawConfig(ctx, p.Home)
	if err != nil || doc["terminal"].(map[string]any)["backend"] != "docker" || doc["model"].(map[string]any)["default"] != "hermes-4" {
		t.Fatalf("doc %v %v", doc, err)
	}
	if level, ok, _ := testBackend().Locked(ctx, p.Home, "terminal.backend"); !ok || level != "group" {
		t.Fatalf("locked %v %v", level, ok)
	}
	var locked *config.ConfigLockedError
	if err := writeKey(p, "terminal.backend", "local"); !errors.As(err, &locked) || locked.Path != "terminal.backend" || locked.LockedBy != "group" || len(p.Patches()) != 0 {
		t.Fatalf("err %v", err)
	}
	if err := writeKey(p, "model.default", "hermes-5"); err != nil {
		t.Fatal(err)
	}
	mustEqualJSON(t, "set", p.Patches()[0].Body["set"], map[string]any{"model.default": "hermes-5"})
}

func TestPlaneGroupLockRefusalMapsToLockedError(t *testing.T) {
	err := writeError(Response{Status: 403, Body: map[string]any{"error": "config_key_locked", "path": "terminal.backend", "lockedBy": "group",
		"lockedByGroupId": "g-sec", "message": "terminal.backend is locked by group g-sec"}})
	var locked *config.ConfigLockedError
	if !errors.As(err, &locked) || locked.Path != "terminal.backend" || locked.LockedBy != "group" || !strings.Contains(err.Error(), "locked by group") {
		t.Fatalf("err %v", err)
	}
}

func TestEtagIsOpaqueAndEchoedVerbatim(t *testing.T) {
	p := setupPlane(t)
	multiGroupPlane(p)
	opaque := `W/"v2:any-bytes/at all"`
	p.EtagOverride = opaque
	ctx := context.Background()
	if _, err := config.ReadRawConfig(ctx, p.Home); err != nil {
		t.Fatal(err)
	}
	b := testBackend()
	st, _ := b.state(ctx, p.Home)
	if b.pollOne(st) || st.lastError != "" {
		t.Fatalf("poll changed %q", st.lastError)
	}
	gets := p.Gets()
	if gets[len(gets)-1].IfNoneMatch != opaque {
		t.Fatalf("etag %q", gets[len(gets)-1].IfNoneMatch)
	}
}

func TestStubPlaneDeepMergeFollowsContract62(t *testing.T) {
	upper := map[string]any{"display": map[string]any{"personality": "concise"}, "model": "a", "tags": []any{1, 2}}
	mustEqualJSON(t, "null over mapping", configplane.DeepMerge(upper, map[string]any{"display": nil}), upper)
	if configplane.DeepMerge(upper, map[string]any{"model": nil})["model"] != nil {
		t.Fatal("null over scalar")
	}
	mustEqualJSON(t, "list", configplane.DeepMerge(upper, map[string]any{"tags": []any{3}})["tags"], []any{3})
}

func TestCASRetryOfADocumentWriteReplaysOnlyItsOwnEdit(t *testing.T) {
	p := setupPlane(t)
	prof := p.Profile("default")
	prof.Values, prof.Version = map[string]any{"display": map[string]any{"personality": "concise", "compact": false}}, 1
	original := request
	raced := false
	request = func(ctx context.Context, method, h, profile, etag string, body any, token string) (Response, error) {
		if method == "PATCH" && !raced {
			raced = true
			p.Mu.Lock()
			prof.Values["display"].(map[string]any)["compact"] = true
			prof.Values["terminal"] = map[string]any{"timeout": 321}
			prof.Version++
			p.Mu.Unlock()
		}
		return original(ctx, method, h, profile, etag, body, token)
	}
	t.Cleanup(func() { request = original })
	if err := setValue(p, "display.personality", "pirate"); err != nil {
		t.Fatal(err)
	}
	mustEqualJSON(t, "values", prof.Values, map[string]any{"display": map[string]any{"personality": "pirate", "compact": true}, "terminal": map[string]any{"timeout": 321}})
	patches := p.Patches()
	if len(patches) != 2 || patches[0].Body["expectedVersion"] != 1 || patches[1].Body["expectedVersion"] != 2 {
		t.Fatalf("patches %v", patches)
	}
	if _, has := patches[1].Body["unset"]; has || strings.Contains(pyStrOf(patches[1].Body), "compact") {
		t.Fatalf("retry body %v", patches[1].Body)
	}
}

func pyStrOf(v any) string {
	s, _ := wireObject(v).(interface{ MarshalJSON() ([]byte, error) }).MarshalJSON()
	return string(s)
}
