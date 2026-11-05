package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Python or SQLite version as three unbounded integers, the shape _version_tuple returns
type VersionTuple [3]*big.Int

// Version tuple of plain integers, missing parts zero and extra parts dropped
func NewVersionTuple(parts ...int64) VersionTuple {
	var v VersionTuple
	for i := range v {
		v[i] = new(big.Int)
		if i < len(parts) {
			v[i].SetInt64(parts[i])
		}
	}
	return v
}

// Lexicographic comparison of two version tuples
func (v VersionTuple) Cmp(o VersionTuple) int {
	for i := range v {
		if c := v[i].Cmp(o[i]); c != 0 {
			return c
		}
	}
	return 0
}

// Iterates a decoded JSON value the way Python's for loop does, the TypeError for non iterables
func pyIterate(v any) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case string:
		out := make([]any, 0, len(x))
		for _, r := range x {
			out = append(out, string(r))
		}
		return out, nil
	case pyObject:
		out := make([]any, 0, len(x))
		for _, kv := range x {
			out = append(out, kv.Key)
		}
		return out, nil
	}
	return nil, &pyConvError{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyTypeName(v))}
}

// Python type name of a decoded JSON value
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case json.Number, int, int64:
		return "int"
	}
	return "float"
}

// int of every part padded with zeros to three and cut at three, with the errors int and iteration raise
func versionTuple(parts any) (VersionTuple, error) {
	items, err := pyIterate(parts)
	if err != nil {
		return VersionTuple{}, err
	}
	v := NewVersionTuple()
	for i, item := range items {
		n, err := pyIntOf(item)
		if err != nil {
			return VersionTuple{}, err
		}
		if i < len(v) {
			v[i] = n
		}
	}
	return v, nil
}

// Whether a SQLite version carries the WAL reset bug, every release from 3.7.0 before the fixed 3.44.6, 3.50.7 and 3.51.3
func IsSQLiteWALResetVulnerable(info VersionTuple) bool {
	return !(info.Cmp(NewVersionTuple(3, 7, 0)) < 0 ||
		info.Cmp(NewVersionTuple(3, 51, 3)) >= 0 ||
		(info.Cmp(NewVersionTuple(3, 50, 7)) >= 0 && info.Cmp(NewVersionTuple(3, 51, 0)) < 0) ||
		(info.Cmp(NewVersionTuple(3, 44, 6)) >= 0 && info.Cmp(NewVersionTuple(3, 45, 0)) < 0))
}

// SQLite details reported by one exact Python executable
type SQLiteRuntimeInfo struct {
	Executable          string
	BasePrefix          string
	PythonVersion       VersionTuple
	SQLiteVersion       VersionTuple
	SQLiteVersionString string
	SQLiteSourceID      string
}

// Whether the interpreter's linked SQLite carries the WAL reset bug
func (i *SQLiteRuntimeInfo) WALResetVulnerable() bool {
	return IsSQLiteWALResetVulnerable(i.SQLiteVersion)
}

const sqliteProbeScript = `
import json, sqlite3, sys
conn = sqlite3.connect(":memory:")
try:
    row = conn.execute("SELECT sqlite_source_id()").fetchone()
finally:
    conn.close()
print(json.dumps({
    "base_prefix": sys.base_prefix, "executable": sys.executable,
    "python_version": list(sys.version_info[:3]), "sqlite_version": list(sqlite3.sqlite_version_info),
    "sqlite_version_string": sqlite3.sqlite_version,
    "sqlite_source_id": str(row[0]) if row and row[0] is not None else "",
}))
`

// Environment with conda, uv, venv and PYTHON overrides stripped so a child interpreter reports its own runtime
func IsolatedInterpreterEnv() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if k == "" && runtime.GOOS == "windows" {
			k2, v2, _ := strings.Cut(v, "=")
			k, v = "="+k2, v2
		}
		if runtime.GOOS == "windows" {
			k = strings.ToUpper(k)
		}
		env[k] = v
	}
	for _, key := range []string{"CONDA_DEFAULT_ENV", "CONDA_PREFIX", "PYTHONHOME", "PYTHONPATH", "UV_PROJECT_ENVIRONMENT",
		"UV_PYTHON", "VIRTUAL_ENV"} {
		delete(env, key)
	}
	return env
}

// Value of a payload key, the TypeError of subscripting a non dict and the KeyError of a missing key
func pyPayloadItem(payload any, key string) (any, error) {
	obj, ok := payload.(pyObject)
	if !ok {
		return nil, &pyConvError{"TypeError", "payload is not subscriptable by str"}
	}
	for _, kv := range obj {
		if kv.Key == key {
			return kv.Value, nil
		}
	}
	return nil, &pyConvError{"KeyError", pyRepr(key)}
}

// Builds the runtime info from a probe payload, any error being one Python would raise from the constructor
func sqliteRuntimeFromPayload(payload any) (*SQLiteRuntimeInfo, error) {
	item := func(key string) (any, error) { return pyPayloadItem(payload, key) }
	executable, err := item("executable")
	if err != nil {
		return nil, err
	}
	basePrefix, err := item("base_prefix")
	if err != nil {
		return nil, err
	}
	pyVersion, err := item("python_version")
	if err != nil {
		return nil, err
	}
	pythonVersion, err := versionTuple(pyVersion)
	if err != nil {
		return nil, err
	}
	sqlVersion, err := item("sqlite_version")
	if err != nil {
		return nil, err
	}
	sqliteVersion, err := versionTuple(sqlVersion)
	if err != nil {
		return nil, err
	}
	versionString, err := item("sqlite_version_string")
	if err != nil {
		return nil, err
	}
	sourceID, err := item("sqlite_source_id")
	if err != nil {
		sourceID = ""
	}
	return &SQLiteRuntimeInfo{
		Executable: pathlibStr(pyStr(executable)), BasePrefix: pathlibStr(pyStr(basePrefix)),
		PythonVersion: pythonVersion, SQLiteVersion: sqliteVersion,
		SQLiteVersionString: pyStr(versionString), SQLiteSourceID: pyStr(sourceID),
	}, nil
}

// Probes SQLite inside the given interpreter, nil when it cannot run, fails, times out or prints an unusable payload
func ProbeSQLiteRuntime(python string, timeout float64) (*SQLiteRuntimeInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c", sqliteProbeScript)
	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil
	}
	env := IsolatedInterpreterEnv()
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, nil
	}
	payload, err := pyLoads(pyDecodeUTF8Replace(stdout.Bytes()))
	if err != nil {
		return nil, nil
	}
	info, err := sqliteRuntimeFromPayload(payload)
	var conv *pyConvError
	if errors.As(err, &conv) && conv.Class == "OverflowError" {
		return nil, err
	}
	if err != nil {
		return nil, nil
	}
	return info, nil
}
