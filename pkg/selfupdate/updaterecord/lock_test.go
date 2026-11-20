package updaterecord

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestMarkerOversizedNumbersAreMalformedAndEmptyMarkersInFlight(t *testing.T) {
	w := World{PID: 1, Now: 100, Alive: func(int) bool { return true }, CTOf: func(int) *float64 { return nil }}
	for _, text := range []string{"99999999999999999999999\n1\n", "1\n" + string(make([]byte, 5000)) + "\n", "4294967296\n1\n", "12\n18446744073709551616\n"} {
		if verdict, _, _ := JudgeMarker([]byte(text), &w); verdict != "malformed" {
			t.Fatalf("%q judged %s", text[:20], verdict)
		}
	}
	if verdict, owner, _ := JudgeMarker([]byte("0004242\n00099\n"), &w); verdict != "live" || owner == nil || *owner != 4242 {
		t.Fatalf("leading zeros judged %s %v", verdict, owner)
	}
	path := filepath.Join(t.TempDir(), MarkerName)
	os.WriteFile(path, nil, 0o644)
	if m := readMarker(path); m == nil || !m.inFlight || m.livePID(w) == nil || *m.livePID(w) != 0 {
		t.Fatalf("fresh empty marker not in flight: %+v", m)
	}
	old := time.Now().Add(-10 * time.Second)
	os.Chtimes(path, old, old)
	if m := readMarker(path); m == nil || m.inFlight {
		t.Fatal("stale empty marker still in flight")
	}
	if readMarker(filepath.Join(t.TempDir(), "absent")) != nil {
		t.Fatal("absent marker read")
	}
}

func TestIncarnationLiveOfThisProcess(t *testing.T) {
	ct := ProcessCreateTime(os.Getpid())
	if ct == nil {
		t.Fatal("creation time of this process unreadable")
	}
	if v := IncarnationLive(os.Getpid(), "ct:"+strconv.FormatFloat(*ct, 'f', 3, 64)); v == nil || !*v {
		t.Fatalf("own identity judged %v", v)
	}
	if v := IncarnationLive(os.Getpid(), nil); v == nil || *v {
		t.Fatal("a no ct claim on our pid judged ours")
	}
	if v := IncarnationLive(0, 1.0); v == nil || *v {
		t.Fatal("pid zero judged live")
	}
	if line := IdentityLine(os.Getpid()); line != "ct:"+strconv.FormatFloat(*ct, 'f', 3, 64) {
		t.Fatalf("identity line %q", line)
	}
	if asCT(" ct:12.5 ") == nil || *asCT(" ct:12.5 ") != 12.5 || asCT("ct:1e3") != nil || asCT([]any{}) != nil {
		t.Fatal("recorded ct spellings misread")
	}
}

func TestCheckoutLockPathFollowsTheCommonGitDir(t *testing.T) {
	root := t.TempDir()
	if path, err := CheckoutLockPath(root); err != nil || path != filepath.Join(root, CheckoutLockName) {
		t.Fatalf("%q %v", path, err)
	}
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	if path, _ := CheckoutLockPath(root); path != filepath.Join(root, ".git", GitCheckoutLockName) {
		t.Fatalf("checkout lock %q", path)
	}
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "h", "profiles", "p"))
	if got := UpdateMarkerPath(); got != filepath.Join(root, "h", MarkerName) {
		t.Fatalf("marker path %q", got)
	}
}
