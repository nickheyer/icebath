package updaterecord

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

type lkCorpus struct {
	Now          float64          `json:"now"`
	OwnCTEpsilon float64          `json:"own_ct_epsilon"`
	CTTolerance  float64          `json:"ct_tolerance"`
	V1MaxAge     float64          `json:"v1_max_age"`
	OurPID       int              `json:"our_pid"`
	OurCT        *float64         `json:"our_ct"`
	Judge        []map[string]any `json:"judge"`
	Release      []map[string]any `json:"release"`
}

func lkLoadCorpus(t *testing.T) lkCorpus {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "update_marker_corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus lkCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	return corpus
}

// World of one corpus case, its own process always alive with its creation time
func lkWorld(corpus lkCorpus, c map[string]any, pidKey, ctKey string) World {
	pid, ct := corpus.OurPID, corpus.OurCT
	if value, ok := c[pidKey]; ok {
		pid = int(value.(float64))
	}
	if value, ok := c[ctKey]; ok {
		ct = nil
		if value != nil {
			v := value.(float64)
			ct = &v
		}
	}
	table := map[int]*float64{}
	for key, value := range c["live"].(map[string]any) {
		n, _ := strconv.Atoi(key)
		table[n] = nil
		if value != nil {
			v := value.(float64)
			table[n] = &v
		}
	}
	table[pid] = ct
	return World{PID: pid, CT: ct, Now: corpus.Now,
		Alive: func(p int) bool { _, ok := table[p]; return ok },
		CTOf:  func(p int) *float64 { return table[p] }}
}

func TestMarkerCorpusConstantsAreTheProductionOnes(t *testing.T) {
	corpus := lkLoadCorpus(t)
	if corpus.OwnCTEpsilon != ownCreateTimeEpsilon || corpus.CTTolerance != CreateTimeToleranceSeconds || corpus.V1MaxAge != UpdateMarkerMaxAgeSeconds {
		t.Fatalf("corpus constants %+v", corpus)
	}
}

func TestMarkerCorpusJudge(t *testing.T) {
	corpus := lkLoadCorpus(t)
	for _, c := range corpus.Judge {
		w := lkWorld(corpus, c, "our_pid", "our_ct")
		verdict, owner, run := JudgeMarker([]byte(c["text"].(string)), &w)
		expect := c["expect"].(map[string]any)
		var gotOwner, gotRun any
		if owner != nil {
			gotOwner = float64(*owner)
		}
		if run != nil {
			gotRun = *run
		}
		if verdict != expect["verdict"] || gotOwner != expect["owner"] || gotRun != expect["run"] {
			t.Errorf("%s: got %s %v %v want %v", c["name"], verdict, gotOwner, gotRun, expect)
		}
	}
}

func TestMarkerCorpusRelease(t *testing.T) {
	corpus := lkLoadCorpus(t)
	for _, c := range corpus.Release {
		w := lkWorld(corpus, c, "releaser_pid", "releaser_ct")
		action, body := releaseDecision([]byte(c["text"].(string)), &w)
		expect := c["expect"].(map[string]any)
		if action != expect["action"] {
			t.Errorf("%s: action %s want %v", c["name"], action, expect)
			continue
		}
		if action == "rewrite" && string(body) != expect["text"] {
			t.Errorf("%s: rewrote %q want %q", c["name"], body, expect["text"])
		}
	}
}

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
