package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

func withNofile(t *testing.T, soft, hard uint64) *[2]uint64 {
	set := &[2]uint64{}
	pg, ps := getNofile, setNofile
	getNofile = func() (uint64, uint64, error) { return soft, hard, nil }
	setNofile = func(s, h uint64) error { set[0], set[1] = s, h; return nil }
	t.Cleanup(func() { getNofile, setNofile = pg, ps })
	return set
}

func TestApplyNofileSoftLimitRaisesWithinHardLimit(t *testing.T) {
	if !nofileSupported {
		if ApplyNofileSoftLimit(map[string]any{}) {
			t.Fatal("raised a limit without the resource module")
		}
		return
	}
	set := withNofile(t, 1024, 2048)
	if !ApplyNofileSoftLimit(map[string]any{}) || *set != [2]uint64{2048, 2048} {
		t.Fatalf("clamped raise %v", *set)
	}
	set = withNofile(t, 1024, rlimInfinity)
	if !ApplyNofileSoftLimit(map[string]any{"runtime": map[string]any{"nofile_soft_limit": 8192}}) || *set != [2]uint64{8192, rlimInfinity} {
		t.Fatalf("configured raise %v", *set)
	}
	for _, cfg := range []map[string]any{
		{"runtime": map[string]any{"nofile_soft_limit": 0}},
		{"runtime": map[string]any{"nofile_soft_limit": false}},
		{"runtime": map[string]any{"nofile_soft_limit": nil}},
		{"runtime": map[string]any{"nofile_soft_limit": "4096"}},
		{"runtime": "off"},
	} {
		withNofile(t, 1024, rlimInfinity)
		if ApplyNofileSoftLimit(cfg) {
			t.Errorf("%v raised the limit", cfg)
		}
	}
	withNofile(t, 8192, rlimInfinity)
	if ApplyNofileSoftLimit(map[string]any{}) {
		t.Fatal("lowered a higher soft limit")
	}
	withNofile(t, rlimInfinity, rlimInfinity)
	if ApplyNofileSoftLimit(map[string]any{}) {
		t.Fatal("changed an unlimited soft limit")
	}
	prev := resourceLimitsLoadConfig
	resourceLimitsLoadConfig = func() (map[string]any, error) {
		return map[string]any{"runtime": map[string]any{"nofile_soft_limit": 5000}}, nil
	}
	defer func() { resourceLimitsLoadConfig = prev }()
	set = withNofile(t, 1024, rlimInfinity)
	if !ApplyNofileSoftLimit(nil) || set[0] != 5000 {
		t.Fatalf("loaded config raise %v", *set)
	}
}

const mallocArenaChildEnv = "ICEBATH_TEST_MALLOC_ARENA_CHILD"

// Child process body of the glibc arena probes, installed only where the probe can run
var mallocArenaChild func(spec string) int

func TestRealConfigLoaderReadsRuntimeNofileSetting(t *testing.T) {
	if !nofileSupported {
		t.Skip("POSIX resource limits only")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("runtime:\n  nofile_soft_limit: 2048\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", dir)
	set := withNofile(t, 256, 4096)
	if !ApplyNofileSoftLimit(nil) || *set != [2]uint64{2048, 4096} {
		t.Fatalf("set %v", *set)
	}
}

func TestUnsupportedPlatformIsASafeNoop(t *testing.T) {
	prev := nofileSupported
	nofileSupported = false
	t.Cleanup(func() { nofileSupported = prev })
	if ApplyNofileSoftLimit(map[string]any{}) {
		t.Fatal("raised without a resource module")
	}
}

func TestInvalidNofileValuesAreSafeNoops(t *testing.T) {
	if !nofileSupported {
		t.Skip("POSIX resource limits only")
	}
	for _, invalid := range []any{true, -1, 4096.0, "4096", struct{}{}} {
		set := withNofile(t, 256, 4096)
		if ApplyNofileSoftLimit(map[string]any{"runtime": map[string]any{"nofile_soft_limit": invalid}}) || *set != [2]uint64{} {
			t.Errorf("%#v changed the limit to %v", invalid, *set)
		}
	}
}

func TestRlimitFailuresAreSafeNoops(t *testing.T) {
	if !nofileSupported {
		t.Skip("POSIX resource limits only")
	}
	set := withNofile(t, 256, 4096)
	setNofile = func(soft, hard uint64) error { return syscall.EPERM }
	if ApplyNofileSoftLimit(map[string]any{}) || *set != [2]uint64{} {
		t.Fatal("setrlimit denial reported a raise")
	}
	set = withNofile(t, 256, 4096)
	getNofile = func() (uint64, uint64, error) { return 0, 0, errors.New("simulated getrlimit failure") }
	if ApplyNofileSoftLimit(map[string]any{}) || *set != [2]uint64{} {
		t.Fatal("getrlimit failure set a limit")
	}
}

func TestArenaMaxSettingPrefersTheExactTunable(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
		ok   bool
	}{
		{map[string]string{"GLIBC_TUNABLES": "glibc.malloc.arena_test=1:glibc.malloc.arena_max=3", "MALLOC_ARENA_MAX": "9"}, "3", true},
		{map[string]string{"GLIBC_TUNABLES": "glibc.malloc.arena_max_x=3:glibc.malloc.arena_max", "MALLOC_ARENA_MAX": "9"}, "9", true},
		{map[string]string{"GLIBC_TUNABLES": "glibc.malloc.arena_max="}, "", true},
		{map[string]string{}, "", false},
	}
	for _, tc := range cases {
		if got, ok := arenaMaxSetting(lookupIn(tc.env)); got != tc.want || ok != tc.ok {
			t.Errorf("%v -> %q %v", tc.env, got, ok)
		}
	}
	for in, want := range map[string]int32{" 4 ": 4, "1_0": 10, "2147483647": cIntMax, "٣": 3} {
		if got, ok := positiveCInt(in); !ok || got != want {
			t.Errorf("%q -> %d %v", in, got, ok)
		}
	}
	for _, in := range []string{"0", "-2", "2147483648", "x", "", "4.0"} {
		if _, ok := positiveCInt(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestLaunchEnvironReadsTheExecTimeBlock(t *testing.T) {
	prev := procSelfEnviron
	t.Cleanup(func() { procSelfEnviron = prev })
	procSelfEnviron = func() ([]byte, error) { return []byte("A=1\x00NOSEP\x00\x00B=x=y\x00C=\xff\x00"), nil }
	want := map[string]string{"A": "1", "B": "x=y", "C": "\xff"}
	if got := launchEnviron(); !reflect.DeepEqual(got, want) {
		t.Fatalf("launch environ %q", got)
	}
	procSelfEnviron = func() ([]byte, error) { return nil, os.ErrNotExist }
	if launchEnviron() != nil {
		t.Fatal("unreadable environ produced a map")
	}
}

func TestServerProcessArgvRecognisesLongLivedServers(t *testing.T) {
	t.Cleanup(func() { pm.SetCommandArgv(nil) })
	pm.SetCommandArgv(nil)
	if _, err := ServerProcessArgv([]string{"serve"}); err == nil {
		t.Fatal("unregistered parser produced an answer")
	}
	pm.SetCommandArgv(func(argv []string) []string {
		out := []string{}
		for i := 0; i < len(argv); i++ {
			if argv[i] == "--profile" {
				i++
				continue
			}
			out = append(out, argv[i])
		}
		return out
	})
	for argv, want := range map[string]bool{"serve": true, "dashboard --port 1": true, "--profile p gateway": true, "gateway run": true,
		"gateway --replace": true, "gateway status": false, "sessions list": false, "": false, "--profile p": false} {
		got, err := ServerProcessArgv(strings.Fields(argv))
		if err != nil || got != want {
			t.Errorf("%q -> %v %v", argv, got, err)
		}
	}
}
