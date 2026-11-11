package gateway

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Runs the respawn filter under a scratch home, a nil own home resolving to that home
func filterRespawn(t *testing.T, candidates []respawnCandidate, ownHome *string) [][]string {
	t.Helper()
	if os.Getenv("ICEBATH_HOME") == "" {
		t.Setenv("ICEBATH_HOME", t.TempDir())
	}
	return filterDashboardRespawnCandidates(context.Background(), candidates, ownHome)
}

func sameArgvs(got [][]string, want ...[]string) bool {
	return slices.EqualFunc(got, want, func(a, b []string) bool { return slices.Equal(a, b) })
}

func TestFilterDashboardRespawnCandidatesSkipsServePortZero(t *testing.T) {
	argv := []string{"python", "-m", "icebath_cli.main", "--profile", "mini-cat", "serve", "--host", "127.0.0.1", "--port", "0"}
	if out := filterRespawn(t, []respawnCandidate{{42, argv, "/home/u/.icebath/profiles/mini-cat"}}, nil); len(out) != 0 {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesSkipsLegacyDashboardPortZero(t *testing.T) {
	argv := []string{"icebath", "--profile", "coder", "dashboard", "--no-open", "--host", "127.0.0.1", "--port", "0"}
	if out := filterRespawn(t, []respawnCandidate{{7, argv, ""}}, nil); len(out) != 0 {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesSkipsServePortEqualsZero(t *testing.T) {
	if out := filterRespawn(t, []respawnCandidate{{1, []string{"icebath", "serve", "--port=0"}, ""}}, nil); len(out) != 0 {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesDedupesIdenticalNormalizedCmdlines(t *testing.T) {
	a := []string{"/usr/bin/python3", "-m", "icebath_cli.main", "dashboard", "--port", "8300"}
	b := []string{"/other/python", "-m", "icebath_cli.main", "dashboard", "--port", "8300"}
	if out := filterRespawn(t, []respawnCandidate{{1, a, ""}, {2, b, ""}}, nil); !sameArgvs(out, a) {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesCapsOnePerProfile(t *testing.T) {
	a := []string{"icebath", "--profile", "coder", "dashboard", "--port", "8300"}
	b := []string{"icebath", "--profile", "coder", "dashboard", "--port", "8301"}
	c := []string{"icebath", "--profile", "writer", "dashboard", "--port", "8302"}
	if out := filterRespawn(t, []respawnCandidate{{1, a, ""}, {2, b, ""}, {3, c, ""}}, nil); !sameArgvs(out, a, c) {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesCapsOnePerHermesHome(t *testing.T) {
	h := "/tmp/icebath-home-a"
	a := []string{"icebath", "dashboard", "--port", "8300"}
	b := []string{"icebath", "dashboard", "--port", "8301"}
	if out := filterRespawn(t, []respawnCandidate{{1, a, h}, {2, b, h}}, &h); !sameArgvs(out, a) {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesProfileFlagAndProfilesHomeShareCap(t *testing.T) {
	a := []string{"icebath", "--profile", "coder", "dashboard", "--port", "8300"}
	b := []string{"icebath", "dashboard", "--port", "8301"}
	if out := filterRespawn(t, []respawnCandidate{{1, a, ""}, {2, b, "/home/u/.icebath/profiles/coder"}}, nil); !sameArgvs(out, a) {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesDefaultProfileSameRootHomeCaps(t *testing.T) {
	a := []string{"icebath", "--profile", "default", "dashboard", "--port", "8300"}
	b := []string{"icebath", "dashboard", "--port", "8301"}
	h := "/home/u/.icebath"
	if out := filterRespawn(t, []respawnCandidate{{1, a, h}, {2, b, h}}, &h); !sameArgvs(out, a) {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesForeignHomeBackendIsNotReplayed(t *testing.T) {
	a := []string{"icebath", "dashboard", "--port", "8300"}
	b := []string{"icebath", "dashboard", "--port", "8301"}
	own := "/home/u/.icebath"
	if out := filterRespawn(t, []respawnCandidate{{1, a, "/home/u/.icebath"}, {2, b, "/work/project/.icebath"}}, &own); !sameArgvs(out, a) {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesSkipsSidecarFixedPortServeOnForeignHome(t *testing.T) {
	argv := []string{"/Users/u/.icebath-sidecar/icebath-agent/venv/bin/python", "-m", "icebath_cli.main", "serve", "--host", "127.0.0.1", "--port", "9118", "--skip-build"}
	own := "/Users/u/.icebath"
	if out := filterRespawn(t, []respawnCandidate{{15364, argv, "/Users/u/.icebath-lifeos"}}, &own); len(out) != 0 {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesMatchingHermesHomeIsKept(t *testing.T) {
	argv := []string{"icebath", "serve", "--host", "127.0.0.1", "--port", "9118"}
	own := "/Users/u/.icebath"
	if out := filterRespawn(t, []respawnCandidate{{15364, argv, "/Users/u/.icebath"}}, &own); !sameArgvs(out, argv) {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesSymlinkedHermesHomeComparesEqual(t *testing.T) {
	tmp := t.TempDir()
	real, link := filepath.Join(tmp, "real-home"), filepath.Join(tmp, "linked-home")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable on this platform")
	}
	argv := []string{"icebath", "serve", "--port", "9118"}
	if out := filterRespawn(t, []respawnCandidate{{15364, argv, real}}, &link); !sameArgvs(out, argv) {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesUnknownHomeStaysEligible(t *testing.T) {
	argv := []string{"icebath", "dashboard", "--port", "8300"}
	own := "/home/u/.icebath"
	if out := filterRespawn(t, []respawnCandidate{{1, argv, ""}}, &own); !sameArgvs(out, argv) {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDashboardRespawnCandidatesOwnHomeDefaultsToGetHermesHome(t *testing.T) {
	t.Setenv("ICEBATH_HOME", "/home/u/.icebath")
	argv := []string{"icebath", "serve", "--port", "9118"}
	if foreign := filterRespawn(t, []respawnCandidate{{1, argv, "/Users/u/.icebath-lifeos"}}, nil); len(foreign) != 0 {
		t.Fatalf("foreign = %v", foreign)
	}
	if own := filterRespawn(t, []respawnCandidate{{1, argv, "/home/u/.icebath"}}, nil); !sameArgvs(own, argv) {
		t.Fatalf("own = %v", own)
	}
}
