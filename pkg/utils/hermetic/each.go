package hermetic

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

// Setup run before one test, the returned teardown run after the cleanups of that test
type Fixture func(t *testing.T) (teardown func())

// Fixtures of the binary in setup order, set once by AroundEachTest
var fixtures []Fixture

// Runs one test inside the fixtures, each teardown ahead of the removal of the test temp dirs
func inside(t *testing.T, run func(t *testing.T)) {
	t.TempDir()
	for _, fixture := range fixtures {
		if teardown := fixture(t); teardown != nil {
			t.Cleanup(teardown)
		}
	}
	run(t)
}

// Runs every top level test of the binary inside the given fixtures
func AroundEachTest(m *testing.M, each ...Fixture) {
	field := reflect.ValueOf(m).Elem().FieldByName("tests")
	if !field.IsValid() || field.Type() != reflect.TypeFor[[]testing.InternalTest]() {
		panic("hermetic: testing.M does not hold its tests as []testing.InternalTest")
	}
	fixtures = each
	tests := *(*[]testing.InternalTest)(unsafe.Pointer(field.UnsafeAddr()))
	for i := range tests {
		run := tests[i].F
		tests[i].F = func(t *testing.T) { inside(t, run) }
	}
}

// Runs a subtest inside the fixtures of the binary, the isolation one parametrized case gets
func Run(t *testing.T, name string, run func(t *testing.T)) bool {
	if fixtures == nil {
		panic("hermetic: Run in a test binary whose TestMain never called AroundEachTest")
	}
	return t.Run(name, func(t *testing.T) { inside(t, run) })
}

// Key and value of one environment entry, a leading equals sign staying part of a Windows drive key
func splitEnviron(entry string) (string, string) {
	if entry == "" {
		return "", ""
	}
	if i := strings.Index(entry[1:], "="); i >= 0 {
		return entry[:i+1], entry[i+2:]
	}
	return entry, ""
}

// Fixture putting the process environment back to its contents before the test
func Environ(t *testing.T) func() {
	before := os.Environ()
	return func() {
		keep := map[string]bool{}
		for _, entry := range before {
			key, value := splitEnviron(entry)
			keep[key] = true
			if current, ok := os.LookupEnv(key); !ok || current != value {
				os.Setenv(key, value)
			}
		}
		for _, entry := range os.Environ() {
			if key, _ := splitEnviron(entry); !keep[key] {
				os.Unsetenv(key)
			}
		}
	}
}

// Fixture returning the process to the working directory it had before the test
func WorkingDir(t *testing.T) func() {
	before, err := os.Getwd()
	if err != nil {
		t.Fatalf("hermetic: working directory unreadable before the test: %v", err)
	}
	return func() {
		if now, err := os.Getwd(); err != nil || now != before {
			if err := os.Chdir(before); err != nil {
				t.Errorf("hermetic: cannot return to %s: %v", before, err)
			}
		}
	}
}
