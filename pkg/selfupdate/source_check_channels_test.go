package selfupdate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Channel fixture home and checkout pinned as ambient home and project root, every release host unreachable
func chCheckHome(t *testing.T, s *chSource) string {
	t.Helper()
	h := filepath.Join(filepath.Dir(s.root), "home")
	os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_INSTALL_ROOT", s.root)
	t.Setenv("ICEBATH_MANAGED", "")
	os.Unsetenv("ICEBATH_MANAGED")
	saved, api, raw, public := checkProjectRoot, sourceGitHubAPI, sourceRawBase, sourcePublicBase
	checkProjectRoot = func() string { return s.root }
	sourceGitHubAPI, sourceRawBase, sourcePublicBase = "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1"
	t.Cleanup(func() { checkProjectRoot, sourceGitHubAPI, sourceRawBase, sourcePublicBase = saved, api, raw, public })
	return h
}

// Channel record the install persisted, read back the way the update command reads it before a write
func chSaved(t *testing.T, s *chSource, h string) map[string]any {
	t.Helper()
	cfg, err := config.RequireReadableConfigBeforeWrite(context.Background(), filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return ChannelRecord(cfg, s.root)
}

func chSetChannel(t *testing.T, s *chSource, name string) {
	t.Helper()
	if _, err := SetInstallChannel(context.Background(), name, s.root); err != nil {
		t.Fatal(err)
	}
}

func chStatus(t *testing.T, s *chSource, h string, channel *string) *pyjson.Object {
	t.Helper()
	return scCheck(t, SourceCheckOptions{InstallRoot: s.root, Home: h, Channel: channel, Force: true})
}

func TestSourceCheckMissingChannelCannotFallBackToMain(t *testing.T) {
	for _, channel := range []string{"preview-not-registered", "stable", "canary"} {
		t.Run(channel, func(t *testing.T) {
			s := newChSource(t)
			h := chCheckHome(t, s)
			chSetChannel(t, s, channel)
			saved := resolveChannel
			resolveChannel = func(context.Context, string, string) (*ChannelResolution, error) {
				return nil, errors.New("Channel does not exist")
			}
			t.Cleanup(func() { resolveChannel = saved })
			status := chStatus(t, s, h, nil)
			if v, _ := status.Get("error"); v != "release-unavailable" {
				t.Fatalf("status %v", status.Vals)
			}
			if _, ok := status.Get("targetSha"); ok {
				t.Fatalf("a missing channel offered a target %v", status.Vals)
			}
		})
	}
}

func TestSourceCheckMainFollowsTheGitBranchWithoutReadingAChannelRecord(t *testing.T) {
	for _, failure := range []string{"not-found", "forbidden", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			s := newChSource(t)
			h := chCheckHome(t, s)
			chSetChannel(t, s, "main")
			var reads []string
			saved := resolveChannel
			resolveChannel = func(_ context.Context, name, _ string) (*ChannelResolution, error) {
				reads = append(reads, name)
				switch failure {
				case "forbidden":
					return nil, &pm.HTTPError{Code: 403, Reason: "Forbidden", URL: "https://x/releases/channels/" + name + ".json"}
				case "timeout":
					return nil, os.ErrDeadlineExceeded
				}
				return nil, &ChannelNotFound{ChannelError{Msg: "Channel object not found: releases/channels/" + name + ".json"}}
			}
			t.Cleanup(func() { resolveChannel = saved })
			status := chStatus(t, s, h, nil)
			if _, failed := status.Get("error"); failed || scGet(t, status, "targetSha") != s.commits[2] || len(reads) != 0 {
				t.Fatalf("status %v reads %v", status.Vals, reads)
			}
		})
	}
}

func TestSourceCheckPassiveCheckReportsRetirementWithoutAdoptingIt(t *testing.T) {
	s := newChSource(t)
	h := chCheckHome(t, s)
	name := "preview-retiring"
	chSetChannel(t, s, name)
	before := chSaved(t, s, h)
	chInstallReader(t, chReaderResult(t, s, name, "stable", OfficialRepository))
	status := chStatus(t, s, h, nil)
	if scGet(t, status, "channel") != name || scGet(t, status, "targetSha") != s.commits[1] || scGet(t, status, "updateAvailable") != true {
		t.Fatalf("status %v", status.Vals)
	}
	retirement, ok := scGet(t, status, "retirement").(*pyjson.Object)
	if !ok || len(retirement.Keys) != 2 || retirement.Vals["destination"] != "stable" || retirement.Vals["sourceOnly"] != true {
		t.Fatalf("retirement %v", status.Vals["retirement"])
	}
	if after := chSaved(t, s, h); !reflect.DeepEqual(after, before) {
		t.Fatalf("a passive check adopted the retirement %v -> %v", before, after)
	}
}

func TestSourceCheckRealHTTPReaderResolvesTaglessBuild(t *testing.T) {
	s := newChSource(t)
	h := chCheckHome(t, s)
	a := chPublishedTree(t)
	name := "http-preview-351"
	a.publishChannelBuild(t, name, strings.Repeat("b", 32), s.commits[1], 1, false)
	status := chStatus(t, s, h, &name)
	if v, _ := status.Get("targetSha"); v != s.commits[1] {
		t.Fatalf("status %v", status.Vals)
	}
}

func TestSourceCheckOfflineRetirementUsesQualifiedBuildBeforeCurrentStable(t *testing.T) {
	for _, fault := range []string{"", "binding", "manifest"} {
		t.Run("fault="+fault, func(t *testing.T) {
			s := newChSource(t)
			h := chCheckHome(t, s)
			a := chPublishedTree(t)
			fixture := a.retiredChannel(t, s)
			chSetChannel(t, s, fixture.name)
			original := chSaved(t, s, h)
			switch fault {
			case "binding":
				head := fixture.retired["destinationHead"].(map[string]any)
				seq, _ := head["sequence"].(interface{ Int64() (int64, error) }).Int64()
				head["sequence"] = seq + 1
				a.put(t, "releases/channels/"+fixture.name+".json", fixture.retired)
			case "manifest":
				key := fixture.qualified["head"].(map[string]any)["manifestKey"].(string)
				a.mu.Lock()
				a.objects[key] = append(a.objects[key], ' ')
				a.mu.Unlock()
			}
			status := chStatus(t, s, h, nil)
			if fault != "" {
				if v, _ := status.Get("error"); v != "release-unavailable" {
					t.Fatalf("status %v", status.Vals)
				}
			} else {
				buildID := fixture.qualified["head"].(map[string]any)["buildId"]
				if v, _ := status.Get("targetSha"); v != s.commits[1] || scGet(t, status, "buildId") != buildID {
					t.Fatalf("status %v", status.Vals)
				}
			}
			if after := chSaved(t, s, h); !reflect.DeepEqual(after, original) {
				t.Fatalf("the check rewrote the channel record %v -> %v", original, after)
			}
		})
	}
}

func TestSourceCheckSourceBranchRecordHasNoBundle(t *testing.T) {
	s := newChSource(t)
	h := chCheckHome(t, s)
	record := jv(t, map[string]any{"schema": 1, "name": "main", "repository": OfficialRepository, "policy": "source-branch",
		"state": "active", "identity": nil, "nextSequence": 2, "head": nil,
		"delivery": map[string]any{"kind": "source-branch", "branch": "main"}}).(*pyjson.Object)
	saved := resolveChannel
	resolveChannel = func(context.Context, string, string) (*ChannelResolution, error) {
		return &ChannelResolution{Requested: pyjson.Clone(record).(*pyjson.Object), Terminal: pyjson.Clone(record).(*pyjson.Object)}, nil
	}
	t.Cleanup(func() { resolveChannel = saved })
	status := chStatus(t, s, h, nil)
	if v, _ := status.Get("targetSha"); v != s.commits[2] {
		t.Fatalf("status %v", status.Vals)
	}
	if _, ok := status.Get("buildId"); ok {
		t.Fatalf("a source branch record carried a build %v", status.Vals)
	}
}

func TestSourceCheckCommitBuildNeverChecksUpstreamOrReusesSourceCache(t *testing.T) {
	for _, passive := range []bool{false, true} {
		root := filepath.Join(t.TempDir(), "app")
		os.MkdirAll(root, 0o755)
		scWrite(t, filepath.Join(root, "install-stamp.json"), `{"source": "commit-build", "distribution": "desktop-app", "payload": "bundled",`+
			` "updateMechanism": "external", "commit": "`+strings.Repeat("a", 40)+`", "displayVersion": "1.2.3+gabcdef12",`+
			` "baseVersion": "1.2.3", "channel": "canary"}`)
		h := filepath.Join(t.TempDir(), "home")
		os.MkdirAll(h, 0o755)
		t.Setenv("ICEBATH_HOME", h)
		t.Setenv("ICEBATH_INSTALL_ROOT", "")
		os.Unsetenv("ICEBATH_INSTALL_ROOT")
		t.Setenv("ICEBATH_REVISION", strings.Repeat("b", 40))
		savedRoot, savedGit, savedAPI := checkProjectRoot, sourceGitRun, sourceGitHubAPI
		checkProjectRoot = func() string { return root }
		sourceGitRun = func(_ context.Context, argv []string, _ string, _ time.Duration) (*sourceGitResult, error) {
			t.Fatalf("a commit build probed git %v", argv)
			return nil, nil
		}
		probe := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			t.Errorf("a commit build probed upstream %s", r.URL)
		}))
		sourceGitHubAPI = probe.URL
		status := scCheck(t, SourceCheckOptions{Passive: passive})
		probe.Close()
		checkProjectRoot, sourceGitRun, sourceGitHubAPI = savedRoot, savedGit, savedAPI
		if behind, ok := status.Get("behind"); !ok || behind != nil {
			t.Fatalf("passive=%v status %v", passive, status.Vals)
		}
	}
}
