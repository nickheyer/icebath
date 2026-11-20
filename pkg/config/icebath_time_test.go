package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Fresh cache and no env zone for the test, both restored afterwards
func freshTimezone(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_TIMEZONE", "")
	os.Unsetenv("ICEBATH_TIMEZONE")
	ResetTimezoneCache()
	t.Cleanup(ResetTimezoneCache)
}

func writeTimezoneHome(t *testing.T, dir, zone string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("timezone: "+zone+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func requireZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := pydatetime.ZoneInfo(name)
	if err != nil {
		t.Skipf("zoneinfo %s unavailable: %v", name, err)
	}
	return loc
}

func TestTimeNowAppliesValidTimezone(t *testing.T) {
	freshTimezone(t)
	requireZone(t, "Asia/Kolkata")
	t.Setenv("ICEBATH_TIMEZONE", "Asia/Kolkata")
	if _, off := TimeNow(context.Background()).Zone(); off != 5*3600+30*60 {
		t.Fatalf("offset = %d", off)
	}
}

func TestInvalidTimezoneFallsBackToServerLocal(t *testing.T) {
	freshTimezone(t)
	t.Setenv("ICEBATH_TIMEZONE", "  Mars/Olympus  ")
	ctx := context.Background()
	if loc, ok := TimeZone(ctx); ok || loc != nil {
		t.Fatalf("TimeZone = %v, %v", loc, ok)
	}
	if got := TimezoneName(ctx); got != "Mars/Olympus" {
		t.Fatalf("TimezoneName = %q", got)
	}
	now := TimeNow(ctx)
	_, off := now.Zone()
	if _, localOff := now.Local().Zone(); off != localOff || now.Location() == time.Local {
		t.Fatal("TimeNow is not the fixed server local offset astimezone gives")
	}
}

func TestSafeStrftimeValidLocaleTextIsByteIdentical(t *testing.T) {
	paris := requireZone(t, "Europe/Paris")
	format := "%a %A %b %B %d %Y %H:%M:%S %Z %z %%Z"
	cases := map[string]time.Time{
		"Tue Tuesday Jul July 14 2026 13:05:00 CEST +0200 %Z":                       time.Date(2026, 7, 14, 13, 5, 0, 0, paris),
		"Tue Tuesday Jul July 14 2026 13:05:00 Hora estándar de Argentina -0300 %Z": time.Date(2026, 7, 14, 13, 5, 0, 0, time.FixedZone("Hora estándar de Argentina", -3*3600)),
	}
	for want, value := range cases {
		got, err := SafeStrftimeChecked(value, format)
		if err != nil || got != want || got != pydatetime.Strftime(value, format) || SafeStrftime(value, format) != want {
			t.Errorf("SafeStrftime = %q, %v, want %q", got, err, want)
		}
	}
}

func TestSafeStrftimeSurrogateZoneNameRendersJSONSafe(t *testing.T) {
	escaped := pystr.SurrogateEscapeDecode([]byte("Paris, Madrid (heure d'\xe9t\xe9)"))
	value := time.Date(2026, 7, 14, 13, 5, 0, 0, time.FixedZone(escaped, 2*3600))
	rendered, err := SafeStrftimeChecked(value, "%a %Y-%m-%d %H:%M %Z %z %%Z")
	if err != nil || !utf8.ValidString(rendered) {
		t.Fatalf("SafeStrftime = %q, %v", rendered, err)
	}
	if !strings.HasPrefix(rendered, "Tue 2026-07-14 13:05 Paris, Madrid (heure d'") || !strings.HasSuffix(rendered, ") +0200 %Z") {
		t.Fatalf("rendered = %q", rendered)
	}
	if got := RepairSurrogates(escaped, "cp1252"); got != "Paris, Madrid (heure d'été)" {
		t.Fatalf("RepairSurrogates = %q", got)
	}
	if got := RepairSurrogates("valid été", ""); got != "valid été" {
		t.Fatalf("valid text changed to %q", got)
	}
	if got := RepairSurrogates(escaped+"é", "cp1252"); got != "Paris, Madrid (heure d'�t�)é" {
		t.Fatalf("mixed text = %q", got)
	}
	if got := RepairSurrogates(escaped, "no-such-codec"); got != "Paris, Madrid (heure d'�t�)" {
		t.Fatalf("unknown codec = %q", got)
	}
	if _, err := SafeStrftimeChecked(value, escaped); err == nil {
		t.Fatal("format holding surrogates rendered")
	}
}

func TestTimezoneCacheIsolatedByActiveProfileConfig(t *testing.T) {
	freshTimezone(t)
	requireZone(t, "Asia/Tokyo")
	requireZone(t, "America/New_York")
	root := t.TempDir()
	first := writeTimezoneHome(t, filepath.Join(root, "first"), "Asia/Tokyo")
	second := writeTimezoneHome(t, filepath.Join(root, "second"), "America/New_York")
	ctx := context.Background()
	for _, step := range []struct{ home, zone string }{{first, "Asia/Tokyo"}, {second, "America/New_York"}, {first, "Asia/Tokyo"}} {
		t.Setenv("ICEBATH_HOME", step.home)
		if loc, ok := TimeZone(ctx); !ok || loc.String() != step.zone {
			t.Fatalf("home %s zone = %v, want %s", step.home, loc, step.zone)
		}
	}
}

func TestMultiplexPrefersRoutedProfileConfigOverEnv(t *testing.T) {
	freshTimezone(t)
	requireZone(t, "Asia/Tokyo")
	root := t.TempDir()
	def := writeTimezoneHome(t, filepath.Join(root, "default"), "America/New_York")
	routed := writeTimezoneHome(t, filepath.Join(root, "routed"), "Asia/Tokyo")
	t.Setenv("ICEBATH_HOME", def)
	t.Setenv("ICEBATH_TIMEZONE", "America/New_York")
	var multiplex atomic.Bool
	prev := IsMultiplexActive
	IsMultiplexActive = func(context.Context) bool { return multiplex.Load() }
	t.Cleanup(func() { IsMultiplexActive = prev })
	ctx := context.Background()
	if got := TimezoneName(ctx); got != "America/New_York" {
		t.Fatalf("single profile = %q", got)
	}
	multiplex.Store(true)
	if got := TimezoneName(ctx); got != "America/New_York" {
		t.Fatalf("default profile turn = %q", got)
	}
	routedCtx := home.WithOverride(ctx, routed)
	if got := TimezoneName(routedCtx); got != "Asia/Tokyo" {
		t.Fatalf("routed profile = %q", got)
	}
	if loc, ok := TimeZone(routedCtx); !ok || loc.String() != "Asia/Tokyo" {
		t.Fatalf("routed zone = %v", loc)
	}
}

func TestConcurrentProfileResolutionNeverMixesZones(t *testing.T) {
	freshTimezone(t)
	zones := map[string]string{"a": "Asia/Tokyo", "b": "America/New_York"}
	root := t.TempDir()
	homes := map[string]string{}
	for key, zone := range zones {
		requireZone(t, zone)
		homes[key] = writeTimezoneHome(t, filepath.Join(root, key), zone)
	}
	var mu sync.Mutex
	var foreign []string
	var start, done sync.WaitGroup
	start.Add(2)
	for key := range zones {
		done.Add(1)
		go func(key string) {
			defer done.Done()
			start.Done()
			start.Wait()
			for range 200 {
				ctx := home.WithOverride(context.Background(), homes[key])
				if loc, ok := TimeZone(ctx); !ok || loc.String() != zones[key] {
					mu.Lock()
					foreign = append(foreign, key+"="+loc.String())
					mu.Unlock()
					return
				}
			}
		}(key)
	}
	done.Wait()
	if len(foreign) != 0 {
		t.Fatalf("foreign timezone observed: %v", foreign)
	}
}

func TestTimezoneHonorsManaged(t *testing.T) {
	freshTimezone(t)
	t.Setenv("TZ", "")
	root := t.TempDir()
	h := writeTimezoneHome(t, filepath.Join(root, "home"), "America/New_York")
	managed := writeTimezoneHome(t, filepath.Join(root, "managed"), "Asia/Tokyo")
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(EnvManagedDir, managed)
	if got := resolveTimezoneName(context.Background()); got != "Asia/Tokyo" {
		t.Fatalf("resolveTimezoneName = %q", got)
	}
}

func TestForeignProcessTickResolvesOwningProfileZone(t *testing.T) {
	freshTimezone(t)
	requireZone(t, "America/New_York")
	root := t.TempDir()
	backend := writeTimezoneHome(t, filepath.Join(root, "backend"), "UTC")
	profile := writeTimezoneHome(t, filepath.Join(root, "north-caribbean"), "America/New_York")
	t.Setenv("ICEBATH_HOME", backend)
	if _, off := TimeNow(context.Background()).Zone(); off != 0 {
		t.Fatalf("backend offset = %d", off)
	}
	ctx := home.WithOverride(context.Background(), profile)
	now := TimeNow(ctx)
	if now.Location().String() != "America/New_York" || TimezoneName(ctx) != "America/New_York" {
		t.Fatalf("owning profile zone = %v", now.Location())
	}
}
