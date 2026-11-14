package selfupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Origin with three commits, the first the release and HEAD two ahead of it
func fsHistory(t *testing.T) (string, []string) {
	origin := filepath.Join(t.TempDir(), "origin")
	os.MkdirAll(origin, 0o755)
	srGit(t, origin, "init", "-q", "-b", "main")
	var shas []string
	for _, n := range []string{"0", "1", "2"} {
		os.WriteFile(filepath.Join(origin, "f"), []byte(n), 0o644)
		srGit(t, origin, "add", "f")
		srGit(t, origin, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", n)
		shas = append(shas, srGit(t, origin, "rev-parse", "HEAD"))
	}
	return origin, shas
}

// Local GitHub answering every comparison with the current status, nil meaning no answer
type fsCompare struct {
	mu     sync.Mutex
	status any
	asked  int
}

func (c *fsCompare) set(status any) {
	c.mu.Lock()
	c.status = status
	c.mu.Unlock()
}

func (c *fsCompare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked++
	if c.status == nil || !strings.HasPrefix(r.URL.Path, "/repos/o/r/compare/") {
		w.WriteHeader(404)
		return
	}
	body, _ := json.Marshal(map[string]any{"status": c.status})
	w.Write(body)
}

func TestSourceShallowCheckoutAheadOfReleaseStaysPut(t *testing.T) {
	for _, fetched := range []bool{false, true} {
		for _, compare := range []any{nil, "ahead"} {
			origin, shas := fsHistory(t)
			tmp := t.TempDir()
			clone := filepath.Join(tmp, "shallow")
			srGit(t, tmp, "clone", "-q", "--depth", "1", "file://"+filepath.ToSlash(origin), clone)
			if fetched {
				srGit(t, origin, "tag", "v0.0.1", shas[0])
				srGit(t, clone, "fetch", "-q", "--depth", "1", "origin", "tag", "v0.0.1")
			}
			github := &fsCompare{status: compare}
			srRoute(t, github)
			ctx := context.Background()
			if head, err := headContaining(ctx, []string{"git"}, clone, shas[0], "o/r"); err != nil || head != shas[2] {
				t.Fatalf("fetched %v compare %v moved to %q %v", fetched, compare, head, err)
			}
			github.set("behind")
			if head, err := headContaining(ctx, []string{"git"}, clone, shas[0], "o/r"); err != nil || head != "" {
				t.Fatalf("a proven behind HEAD stayed at %q %v", head, err)
			}
		}
	}
}

func TestSourceFullHistoryBehindReleaseIsProof(t *testing.T) {
	origin, shas := fsHistory(t)
	tmp := t.TempDir()
	clone := filepath.Join(tmp, "full")
	srGit(t, tmp, "clone", "-q", origin, clone)
	srGit(t, clone, "checkout", "-q", shas[0])
	github := &fsCompare{status: "ahead"}
	srRoute(t, github)
	ctx := context.Background()
	if head, err := headContaining(ctx, []string{"git"}, clone, shas[2], "o/r"); err != nil || head != "" {
		t.Fatalf("full history behind answered %q %v", head, err)
	}
	if head, err := headContaining(ctx, []string{"git"}, clone, shas[0], "o/r"); err != nil || head != shas[0] {
		t.Fatalf("containing HEAD answered %q %v", head, err)
	}
	if github.asked != 0 {
		t.Fatalf("proof asked GitHub %d times", github.asked)
	}
}

func TestSourceAProbeThatNeverAnsweredLeavesTheRelationUnknown(t *testing.T) {
	for _, tc := range []struct {
		compare   string
		keepsHead bool
	}{{"ahead", true}, {"behind", false}} {
		origin, shas := fsHistory(t)
		var timedOut [][]string
		previous := runSourceGit
		runSourceGit = func(ctx context.Context, argv []string, cwd string, timeout time.Duration) (*sourceGitResult, error) {
			if len(argv) > 1 && argv[0] == "git" && argv[1] == "merge-base" {
				timedOut = append(timedOut, argv)
				return nil, &sourceGitTimeout{Argv: argv, Timeout: timeout}
			}
			return previous(ctx, argv, cwd, timeout)
		}
		t.Cleanup(func() { runSourceGit = previous })
		srRoute(t, &fsCompare{status: tc.compare})
		want := ""
		if tc.keepsHead {
			want = shas[2]
		}
		if head, err := headContaining(context.Background(), []string{"git"}, origin, shas[2], "o/r"); err != nil || head != want {
			t.Fatalf("compare %s answered %q %v", tc.compare, head, err)
		}
		if len(timedOut) != 1 || strings.Join(timedOut[0], " ") != "git merge-base --is-ancestor "+shas[2]+" "+shas[2] {
			t.Fatalf("timed out probes %v", timedOut)
		}
		runSourceGit = previous
	}
}
