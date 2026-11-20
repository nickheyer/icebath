package webhooksubs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func storeHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func route(pairs ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

func TestLoadCorruptedOrNonObjectFileReadsEmpty(t *testing.T) {
	h := storeHome(t)
	for _, body := range []string{"broken{{{", "[1, 2]", "\xff\xfe{}", ""} {
		os.WriteFile(Path(h), []byte(body), 0o600)
		if got := Load(h); got.Len() != 0 {
			t.Fatalf("%q read as %v", body, got.Keys)
		}
	}
	os.WriteFile(Path(h), []byte("\xef\xbb\xbf{\"b\": {}, \"a\": {}}"), 0o600)
	if got := Load(h); strings.Join(got.Keys, ",") != "b,a" {
		t.Fatalf("BOM file or key order lost: %v", got.Keys)
	}
	os.Remove(Path(h))
	if got := LoadUnlocked(h); got.Len() != 0 {
		t.Fatal("missing file must read empty")
	}
}

func TestSaveNarrowsExistingBroadSecretFileMode(t *testing.T) {
	h := storeHome(t)
	os.WriteFile(Path(h), []byte(`{"old": {"secret": "stale", "prompt": "x"}}`), 0o644)
	os.Chmod(Path(h), 0o644)
	if err := Mutate(h, func(s *pyjson.Object) error {
		s.Set("demo", route("secret", "FRESH", "prompt", "x"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(Path(h))
	data, _ := os.ReadFile(Path(h))
	if st.Mode().Perm() != 0o600 || !strings.Contains(string(data), "FRESH") {
		t.Fatalf("mode %v body %s", st.Mode().Perm(), data)
	}
	want := "{\n  \"old\": {\n    \"secret\": \"stale\",\n    \"prompt\": \"x\"\n  },\n  \"demo\": {\n    \"secret\": \"FRESH\",\n    \"prompt\": \"x\"\n  }\n}"
	if string(data) != want {
		t.Fatalf("json.dump indent 2 order mismatch:\n%s", data)
	}
	if _, err := os.Stat(LockPath(h)); err != nil {
		t.Fatal("lock file not created beside the store")
	}
}

func TestMutateErrorPublishesNothing(t *testing.T) {
	h := storeHome(t)
	boom := errors.New("no such route")
	if err := Mutate(h, func(s *pyjson.Object) error { s.Set("x", 1); return boom }); err != boom {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(Path(h)); !os.IsNotExist(err) {
		t.Fatal("failed mutation wrote the store")
	}
}

func TestMutateSerializesConcurrentWriters(t *testing.T) {
	h := storeHome(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Mutate(h, func(s *pyjson.Object) error {
				s.Set(fmt.Sprintf("r%02d", i), route("n", int64(i)))
				return nil
			}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got := Load(h).Len(); got != 16 {
		t.Fatalf("lost updates, %d routes", got)
	}
}

func TestStaleReplaceCannotRestoreRemovedOrDisabledRoute(t *testing.T) {
	h := storeHome(t)
	if _, err := Replace(h, "notifier", route("secret", "original"), Missing); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := Load(h).Get("notifier")
	if err := Mutate(h, func(s *pyjson.Object) error { s.Delete("notifier"); return nil }); err != nil {
		t.Fatal(err)
	}
	_, err := Replace(h, "notifier", route("description", "stale update"), snapshot)
	var conflict *SubscriptionMutationConflict
	if !errors.As(err, &conflict) || conflict.Error() != "Webhook subscription 'notifier' changed concurrently; retry the operation." {
		t.Fatalf("stale replace after removal: %v", err)
	}
	if _, ok := Load(h).Get("notifier"); ok {
		t.Fatal("stale update recreated a removed route")
	}
	if _, err := Replace(h, "notifier", route("secret", "replacement"), Missing); err != nil {
		t.Fatal(err)
	}
	if _, err := Replace(h, "notifier", route("secret", "dup"), Missing); !errors.As(err, &conflict) {
		t.Fatal("create over an existing route must conflict")
	}
	snapshot, _ = Load(h).Get("notifier")
	Mutate(h, func(s *pyjson.Object) error {
		r, _ := s.Get("notifier")
		r.(*pyjson.Object).Set("enabled", false)
		return nil
	})
	if _, err := Replace(h, "notifier", route("description", "stale update"), snapshot); !errors.As(err, &conflict) {
		t.Fatalf("stale replace after disable: %v", err)
	}
	r, _ := Load(h).Get("notifier")
	if v, _ := r.(*pyjson.Object).Get("enabled"); v != false {
		t.Fatal("disabled route re-enabled")
	}
	current, _ := Load(h).Get("notifier")
	published, err := Replace(h, "notifier", route("secret", "next"), current)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := published.Get("enabled"); v != false {
		t.Fatal("general update lifted an explicit enabled false")
	}
}

func TestReplaceComparesWithPythonEquality(t *testing.T) {
	h := storeHome(t)
	os.WriteFile(Path(h), []byte(`{"r": {"a": 1, "b": [true, 2.0]}}`), 0o600)
	expected := route("b", []any{int64(1), int64(2)}, "a", 1.0)
	if _, err := Replace(h, "r", route("a", int64(2)), expected); err != nil {
		t.Fatalf("dict equality ignores order and compares numbers numerically: %v", err)
	}
}

func TestApplyPlainKeepsFileOrderAndAppendsNewKeysSorted(t *testing.T) {
	dst := route("zeta", route("y", int64(1), "x", int64(2)), "alpha", route("k", "v"), "gone", int64(1))
	plain := pyjson.Plain(dst).(map[string]any)
	delete(plain, "gone")
	plain["new2"] = map[string]any{"b": 1, "a": 2}
	plain["new1"] = "v"
	plain["zeta"].(map[string]any)["w"] = int64(3)
	ApplyPlain(dst, plain)
	if strings.Join(dst.Keys, ",") != "zeta,alpha,new1,new2" {
		t.Fatalf("top order %v", dst.Keys)
	}
	z, _ := dst.Get("zeta")
	if strings.Join(z.(*pyjson.Object).Keys, ",") != "y,x,w" {
		t.Fatalf("nested order %v", z.(*pyjson.Object).Keys)
	}
}

func TestBaseURLMapsWildcardHostsToLocalhost(t *testing.T) {
	h := storeHome(t)
	ctx := context.Background()
	for _, host := range []string{"", "host: ''", "host: 0.0.0.0", "host: '::'"} {
		body := "platforms:\n  webhook:\n    extra:\n      port: 9123\n"
		if host != "" {
			body += "      " + host + "\n"
		}
		os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600)
		if got := BaseURL(ctx, h); got != "http://localhost:9123" {
			t.Fatalf("%q gave %s", host, got)
		}
	}
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("platforms:\n  webhook:\n    enabled: true\n    extra:\n      host: 'fe80::1'\n"), 0o600)
	if got := BaseURL(ctx, h); got != "http://[fe80::1]:8644" {
		t.Fatalf("ipv6 host %s", got)
	}
	if !WebhookEnabled(ctx, h) {
		t.Fatal("enabled flag not read")
	}
	if got := RouteURL(ctx, h, "gh", route("profile", "work")); got != "http://[fe80::1]:8644/p/work/webhooks/gh" {
		t.Fatalf("profile route %s", got)
	}
	if got := RouteURL(ctx, h, "gh", route()); got != "http://[fe80::1]:8644/webhooks/gh" {
		t.Fatalf("default route %s", got)
	}
	os.Remove(filepath.Join(h, "config.yaml"))
	if WebhookEnabled(ctx, h) || len(WebhookConfig(ctx, h)) != 0 {
		t.Fatal("unconfigured webhook platform must read empty and disabled")
	}
}
