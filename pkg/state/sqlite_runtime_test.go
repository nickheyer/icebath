package state

import (
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"testing"
)

func TestSQLiteWALResetVulnerableRanges(t *testing.T) {
	for _, c := range []struct {
		version []int64
		want    bool
	}{
		{[]int64{3, 6, 99}, false}, {[]int64{3, 7, 0}, true}, {[]int64{3, 44, 5}, true}, {[]int64{3, 44, 6}, false},
		{[]int64{3, 44, 99}, false}, {[]int64{3, 45, 0}, true}, {[]int64{3, 50, 6}, true}, {[]int64{3, 50, 7}, false},
		{[]int64{3, 50, 99}, false}, {[]int64{3, 51, 0}, true}, {[]int64{3, 51, 2}, true}, {[]int64{3, 51, 3}, false},
		{[]int64{4}, false}, {[]int64{3}, false}, {[]int64{3, 46, 1, 9}, true}, {nil, false},
	} {
		if got := IsSQLiteWALResetVulnerable(NewVersionTuple(c.version...)); got != c.want {
			t.Fatalf("%v vulnerable %v want %v", c.version, got, c.want)
		}
		info := SQLiteRuntimeInfo{SQLiteVersion: NewVersionTuple(c.version...)}
		if info.WALResetVulnerable() != c.want {
			t.Fatalf("runtime info %v", c.version)
		}
	}
}

func TestVersionTupleConvertsLikeInt(t *testing.T) {
	for _, c := range []struct {
		parts any
		want  []int64
	}{
		{[]any{json.Number("3"), json.Number("45"), json.Number("1")}, []int64{3, 45, 1}},
		{[]any{json.Number("3")}, []int64{3, 0, 0}},
		{[]any{json.Number("3"), json.Number("1"), json.Number("2"), json.Number("9")}, []int64{3, 1, 2}},
		{"312", []int64{3, 1, 2}},
		{[]any{"3", 4.9, true}, []int64{3, 4, 1}},
		{pyObject{{Key: "7"}, {Key: "8"}}, []int64{7, 8, 0}},
		{[]any{}, []int64{0, 0, 0}},
	} {
		got, err := versionTuple(c.parts)
		if err != nil || got.Cmp(NewVersionTuple(c.want...)) != 0 {
			t.Fatalf("%v gave %v %v", c.parts, got, err)
		}
	}
	for _, c := range []struct {
		parts any
		class string
	}{
		{"3.1", "ValueError"}, {json.Number("5"), "TypeError"}, {nil, "TypeError"}, {[]any{nil}, "TypeError"},
		{[]any{math.NaN()}, "ValueError"}, {[]any{math.Inf(1)}, "OverflowError"}, {[]any{[]any{}}, "TypeError"},
	} {
		_, err := versionTuple(c.parts)
		var conv *pyConvError
		if !errors.As(err, &conv) || conv.Class != c.class {
			t.Fatalf("%v raised %v want %s", c.parts, err, c.class)
		}
	}
	huge, _ := versionTuple([]any{json.Number("99999999999999999999")})
	if huge.Cmp(NewVersionTuple(math.MaxInt64)) <= 0 || IsSQLiteWALResetVulnerable(huge) {
		t.Fatal("versions stay unbounded like Python ints")
	}
}

func TestIsolatedInterpreterEnvStripsOverrides(t *testing.T) {
	stripped := []string{"CONDA_DEFAULT_ENV", "CONDA_PREFIX", "PYTHONHOME", "PYTHONPATH", "UV_PROJECT_ENVIRONMENT", "UV_PYTHON", "VIRTUAL_ENV"}
	for _, k := range stripped {
		t.Setenv(k, "/override")
	}
	t.Setenv("ICEBATH_KEEP_ME", "kept")
	env := IsolatedInterpreterEnv()
	for _, k := range stripped {
		if _, ok := env[k]; ok {
			t.Fatalf("%s survived", k)
		}
	}
	if env["ICEBATH_KEEP_ME"] != "kept" {
		t.Fatal("an unrelated variable was dropped")
	}
}

func TestProbeOfAMissingInterpreterIsNil(t *testing.T) {
	info, err := ProbeSQLiteRuntime(filepath.Join(t.TempDir(), "no-python"), 30)
	if info != nil || err != nil {
		t.Fatalf("%v %v", info, err)
	}
}

func TestRuntimeInfoFromPayloadShapes(t *testing.T) {
	payload, err := pyLoads(`{"base_prefix": "/usr//lib/", "executable": "/usr/bin/python3", "python_version": [3, 12, 4],
		"sqlite_version": [3, 45, 1], "sqlite_version_string": "3.45.1"}`)
	if err != nil {
		t.Fatal(err)
	}
	info, err := sqliteRuntimeFromPayload(payload)
	if err != nil || info.BasePrefix != "/usr/lib" || info.Executable != "/usr/bin/python3" || info.SQLiteSourceID != "" ||
		info.PythonVersion.Cmp(NewVersionTuple(3, 12, 4)) != 0 || info.SQLiteVersionString != "3.45.1" || !info.WALResetVulnerable() {
		t.Fatalf("%+v %v", info, err)
	}
	for _, text := range []string{`[]`, `"x"`, `{"executable": "x"}`, `{"executable": 1, "base_prefix": 2, "python_version": 3}`} {
		payload, _ := pyLoads(text)
		if _, err := sqliteRuntimeFromPayload(payload); err == nil {
			t.Fatalf("%s built an info", text)
		}
	}
}
