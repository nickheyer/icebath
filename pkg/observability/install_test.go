package observability

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

var fullV4 = map[string]any{"behind_bucket": "unknown", "gpu_class": "nvidia", "local_model_provider_used": "yes",
	"ram_bucket": "16g_to_32g", "release_channel": "stable", "version_age_bucket": "lt_7d"}

// Installed version reader answering one commit, its date and branch
func withVersion(t *testing.T, commitDate *int64, branch *string) {
	t.Helper()
	saved := installVersionInfo
	commit := strings.Repeat("a", 40)
	installVersionInfo = func() (updaterecord.VersionInfo, error) {
		return updaterecord.VersionInfo{Commit: &commit, CommitDate: commitDate, Branch: branch}, nil
	}
	t.Cleanup(func() { installVersionInfo = saved })
}

func withProjectRoot(t *testing.T, root string) {
	t.Helper()
	saved := installProjectRoot
	installProjectRoot = func() string { return root }
	t.Cleanup(func() { installProjectRoot = saved })
}

func TestInstallSnapshotAcceptsV4FieldsAndRowsCountedBeforeThem(t *testing.T) {
	legacy := map[string]any{"cron_job_count_bucket": "0", "display_language": "en", "install_age_bucket": "lt_1h",
		"main_provider": "openrouter", "mcp_server_count_bucket": "0", "memory_provider": "builtin",
		"messaging_platform_count_bucket": "0", "plugin_count_bucket": "0", "profile_count_bucket": "1",
		"skill_count_bucket": "0", "terminal_backend": "local"}
	merged := func(extra ...map[string]any) map[string]any {
		out := maps.Clone(legacy)
		for _, e := range extra {
			maps.Copy(out, e)
		}
		return out
	}
	if !CounterDimensionsAreValid(InstallSnapshotMetric, legacy) || !CounterDimensionsAreValid(InstallSnapshotMetric, merged(fullV4)) {
		t.Fatal("legacy or full v4 snapshot rejected")
	}
	if CounterDimensionsAreValid(InstallSnapshotMetric, merged(fullV4, map[string]any{"gpu_class": "rtx-4090"})) ||
		CounterDimensionsAreValid(InstallSnapshotMetric, merged(map[string]any{"ram_bucket": "lt_8g"})) {
		t.Fatal("off vocabulary or partial v4 snapshot accepted")
	}
}

func TestRamBucketsLandOnTheNominalSize(t *testing.T) {
	saved := installRAMTotalBytes
	t.Cleanup(func() { installRAMTotalBytes = saved })
	for _, c := range []struct {
		gib    float64
		bucket string
	}{{3.8, "lt_8g"}, {7.6, "8g_to_16g"}, {15.5, "16g_to_32g"}, {31.2, "32g_to_64g"}, {62.6, "64g_to_128g"}, {125.7, "gte_128g"}, {512, "gte_128g"}} {
		total := int64(c.gib * gib)
		installRAMTotalBytes = func() int64 { return total }
		if got, err := RamBucket(); err != nil || got != c.bucket {
			t.Fatalf("%v GiB read %s %v", c.gib, got, err)
		}
	}
}

func TestRamUnknownWhenTheOSDoesNotSay(t *testing.T) {
	saved := installRAMTotalBytes
	t.Cleanup(func() { installRAMTotalBytes = saved })
	installRAMTotalBytes = func() int64 { return 0 }
	if got, err := RamBucket(); err != nil || got != "unknown" {
		t.Fatalf("ram %s %v", got, err)
	}
}

func TestMeminfoTotalIsParsedInBytes(t *testing.T) {
	if got := hostinfo.ParseMeminfoTotal("MemFree: 12 kB\nMemTotal:       65681892 kB\n"); got != 65681892*1024 {
		t.Fatalf("total %d", got)
	}
	if got := hostinfo.ParseMeminfoTotal("garbage"); got != 0 {
		t.Fatalf("garbage %d", got)
	}
}

func TestGpuVendorPriorityPrefersTheDiscreteCard(t *testing.T) {
	for _, c := range []struct {
		vendors []string
		gpu     string
	}{{[]string{"0x8086", "0x10de"}, "nvidia"}, {[]string{"0x8086", "0x1002"}, "amd"}, {[]string{"0x8086"}, "intel"},
		{[]string{"0x1af4"}, "none"}, {nil, "none"}, {[]string{"10DE"}, "nvidia"}, {[]string{""}, "unknown"}, {[]string{"", "0x10de"}, "nvidia"}} {
		if got := hostinfo.ClassifyGPUVendors(c.vendors); got != c.gpu {
			t.Fatalf("%v read %s", c.vendors, got)
		}
	}
}

func TestVersionAgeIsTheInstalledCommitAge(t *testing.T) {
	const now = 1_800_000_000
	for _, c := range []struct {
		days   float64
		bucket string
	}{{0.5, "lt_7d"}, {8, "7d_to_30d"}, {45, "30d_to_90d"}, {400, "gte_90d"}} {
		date := int64(now - c.days*86_400)
		withVersion(t, &date, nil)
		if got, err := InstallVersionAgeBucket(now); err != nil || got != c.bucket {
			t.Fatalf("%v days read %s %v", c.days, got, err)
		}
	}
}

func TestVersionAgeUnknownWithoutACommitDate(t *testing.T) {
	withVersion(t, nil, nil)
	if got, err := InstallVersionAgeBucket(0); err != nil || got != "unknown" {
		t.Fatalf("age %s %v", got, err)
	}
}

func writeUpdateCache(t *testing.T, homeDir, root string, behind int, head string, ts float64) {
	t.Helper()
	path := filepath.Join(homeDir, "source-checks", tools.InstallID(root)+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"ts": %v, "identity": {"head": %q}, "status": {"behind": %d}}`, ts, head, behind)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBehindComesOnlyFromAFreshCachedCheckOfThisRevision(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "checkout")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	withProjectRoot(t, root)
	withVersion(t, nil, nil)
	homeDir := filepath.Join(dir, "home")
	now := pyTime()
	ctx := context.Background()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	steps := []struct {
		write  func()
		bucket string
	}{
		{func() {}, "unknown"},
		{func() { writeUpdateCache(t, homeDir, root, 4, a, now-3600) }, "3_to_5"},
		{func() { writeUpdateCache(t, homeDir, root, 0, a, now-60) }, "0"},
		{func() { writeUpdateCache(t, homeDir, root, 4, b, now-60) }, "unknown"},
		{func() { writeUpdateCache(t, homeDir, root, 4, a, now-8*86_400) }, "unknown"},
	}
	for i, step := range steps {
		step.write()
		if got, err := BehindBucket(ctx, homeDir, now); err != nil || got != step.bucket {
			t.Fatalf("step %d read %s %v", i, got, err)
		}
	}
}

func TestBehindNeverTouchesTheNetwork(t *testing.T) {
	dir := t.TempDir()
	withProjectRoot(t, dir)
	if got, err := BehindBucket(context.Background(), dir, 0); err != nil || got != "unknown" {
		t.Fatalf("behind %s %v", got, err)
	}
}

func TestSnapshotFieldsDegradeToUnknownWhenAReaderBreaks(t *testing.T) {
	saved := installV4Readers
	t.Cleanup(func() { installV4Readers = saved })
	boom := func(context.Context) (string, error) { return "", errors.New("unreadable") }
	boomCfg := func(context.Context, map[string]any) (string, error) { return "", errors.New("unreadable") }
	installV4Readers.behind, installV4Readers.gpu, installV4Readers.ram, installV4Readers.version = boom, boom, boom, boom
	installV4Readers.local, installV4Readers.channel = boomCfg, boomCfg
	got := InstallV4SnapshotFields(context.Background(), map[string]any{})
	want := map[string]string{"behind_bucket": "unknown", "gpu_class": "unknown", "local_model_provider_used": "no",
		"ram_bucket": "unknown", "release_channel": "unknown", "version_age_bucket": "unknown"}
	if !maps.Equal(got, want) {
		t.Fatalf("fields %v", got)
	}
}

func TestHostFactsAreCachedAndInVocabulary(t *testing.T) {
	hostinfo.ClearCaches()
	first := hostinfo.GPUClass()
	if !GPUClasses.Has(first) || hostinfo.GPUClass() != first {
		t.Fatalf("gpu class %s", first)
	}
	if hostinfo.RAMTotalBytes() < 0 {
		t.Fatal("negative ram")
	}
}
