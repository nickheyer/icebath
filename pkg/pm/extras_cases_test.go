package pm

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRuntimeMarkerEvaluationAnswersForTheGivenEnvironment(t *testing.T) {
	isolateHome(t)
	var out []bool
	for _, marker := range []string{"sys_platform == 'linux'", "sys_platform == 'win32'"} {
		ok, err := evaluateGate(context.Background(), marker, map[string]string{"sys_platform": "linux"})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ok)
	}
	if !reflect.DeepEqual(out, []bool{true, false}) {
		t.Fatalf("got %v", out)
	}
}

func TestGateMarkerThatFailsToEvaluateIsUngated(t *testing.T) {
	isolateHome(t)
	for _, marker := range []string{"sys_platform ==", "sys_platform < 'linux' and", "python_version >= 'x' xor os_name == 'nt'", "'a' === extras"} {
		if ok, err := evaluateGate(context.Background(), marker, map[string]string{"sys_platform": "linux"}); err != nil || !ok {
			t.Fatalf("%q gave %v %v, a marker that fails must leave the extra ungated", marker, ok, err)
		}
	}
}

func TestGateFallsBackOnTheInterpreterMarkerEnvironment(t *testing.T) {
	tmp := isolateHome(t)
	defaults := svHostInterpreter(t, sttMkdir(t, filepath.Join(tmp, "repo")))
	ctx := context.Background()
	environment := map[string]string{"sys_platform": "linux"}
	if ok, err := evaluateGate(ctx, "python_full_version == '"+defaults["python_full_version"]+"'", environment); err != nil || !ok {
		t.Fatalf("interpreter version gate gave %v %v", ok, err)
	}
	if ok, err := evaluateGate(ctx, "python_full_version < '"+defaults["python_full_version"]+"' and sys_platform == 'linux'", environment); err != nil || ok {
		t.Fatalf("older interpreter gate gave %v %v", ok, err)
	}
	if ok, err := evaluateGate(ctx, "implementation_name == '"+defaults["implementation_name"]+"' and sys_platform == 'win32'", environment); err != nil || ok {
		t.Fatalf("the given environment must override the interpreter defaults, gave %v %v", ok, err)
	}
}

func TestGateWithoutAnInterpreterReportsTheMissingToolchain(t *testing.T) {
	tmp := isolateHome(t)
	repo := sttMkdir(t, filepath.Join(tmp, "repo"))
	peSwap(t, &repoRoot, func() string { return repo })
	ctx := context.Background()
	if ok, err := evaluateGate(ctx, "sys_platform == 'linux'", map[string]string{"sys_platform": "linux"}); err != nil || !ok {
		t.Fatalf("a gate over the given environment needs no interpreter, gave %v %v", ok, err)
	}
	_, err := evaluateGate(ctx, "python_version >= '3'", map[string]string{"sys_platform": "linux"})
	var ie *InstallError
	if !errors.As(err, &ie) {
		t.Fatalf("a gate needing interpreter defaults without one gave %v", err)
	}
}
