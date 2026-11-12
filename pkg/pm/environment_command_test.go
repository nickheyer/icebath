package pm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Side environment build double that reports progress on descriptor 1 and through print the way PM and uv children do
func ecNoisy(result string, calls *[]EnsureEnvironmentOptions) func(context.Context, EnsureEnvironmentOptions) (string, error) {
	return func(ctx context.Context, opts EnsureEnvironmentOptions) (string, error) {
		*calls = append(*calls, opts)
		ecWriteStdoutFD("  -> Installing Python dependencies\n")
		fmt.Fprintln(SyncOutput(ctx), "  ok Installing Python dependencies")
		return result, nil
	}
}

// Runs pm environment through Main with the arguments argparse builds from the given flags
func ecMain(args EnvironmentArgs) (int, error) {
	return Main(context.Background(), func(ctx context.Context) (int, error) { return CmdEnvironment(ctx, args) })
}

// Checkout the command reads its extras from, the upstream project as Python's PROJECT
func ecProject(t *testing.T) {
	t.Helper()
	isolateHome(t)
	references := peReferences(t, "pyproject.toml")
	peSwap(t, &repoRoot, func() string { return references })
}

func TestStdoutIsOnlyTheInterpreterForCommandSubstitution(t *testing.T) {
	ecProject(t)
	python := filepath.Join(t.TempDir(), "gen-x", "venv", "bin", "python")
	var calls []EnsureEnvironmentOptions
	peSwap(t, &pmEnsureEnvironment, ecNoisy(python, &calls))
	var code int
	var err error
	out, stderr := ecCaptureFDs(t, func() {
		code, err = ecMain(EnvironmentArgs{Name: "documents", Extra: []string{"documents", "documents-pdf"}})
	})
	if code != 0 || err != nil {
		t.Fatalf("main %d %v, stderr %q", code, err, stderr)
	}
	if out != python+"\n" || !strings.Contains(stderr, "Installing Python dependencies") {
		t.Fatalf("stdout %q stderr %q", out, stderr)
	}
	if len(calls) == 0 {
		t.Fatal("no environment built")
	}
	call := calls[0]
	if call.Name != "documents" || !call.Explicit || call.Root != "" || call.Timeout != 0 || call.Executable != "" {
		t.Fatalf("call %+v", call)
	}
	pinned := map[string]bool{}
	for _, requirement := range call.Requirements {
		pinned[strings.SplitN(requirement, "==", 2)[0]] = true
		if !strings.Contains(requirement, "==") {
			t.Fatalf("unpinned requirement %q", requirement)
		}
	}
	for _, want := range []string{"python-docx", "openpyxl", "python-pptx", "reportlab", "pypdf", "pdfplumber", "pypdfium2"} {
		if !pinned[want] {
			t.Fatalf("pins %v lack %s", call.Requirements, want)
		}
	}
}

func TestExtraExpandsSelfReferencesAndRejectsUnknown(t *testing.T) {
	tmp := t.TempDir()
	peWrite(t, filepath.Join(tmp, "pyproject.toml"), "[project]\nname = \"demo\"\nversion = \"0\"\n"+
		"[project.optional-dependencies]\n"+
		"a = [\"pkg-a==1.0\"]\nb = [\"demo[a]\", \"pkg-b==2.0\"]\n")
	got, err := extraRequirements(tmp, []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if !reflect.DeepEqual(got, []string{"pkg-a==1.0", "pkg-b==2.0"}) {
		t.Fatalf("requirements %q", got)
	}
	_, err = extraRequirements(tmp, []string{"missing"})
	var py *PyError
	if !errors.As(err, &py) || py.Class != "ValueError" || !strings.Contains(py.Message, "not an extra") {
		t.Fatalf("missing extra %#v", err)
	}
}

func TestRequirementsSelectAnAdHocEnvironment(t *testing.T) {
	ecProject(t)
	python := filepath.Join(t.TempDir(), "python")
	var calls []EnsureEnvironmentOptions
	peSwap(t, &pmEnsureEnvironment, ecNoisy(python, &calls))
	var code int
	var err error
	out, _ := ecCaptureFDs(t, func() {
		code, err = ecMain(EnvironmentArgs{Name: "pdf-pymupdf", Requirement: []string{"pymupdf>=1.26,<3"}})
	})
	if code != 0 || err != nil || out != python+"\n" {
		t.Fatalf("main %d %v stdout %q", code, err, out)
	}
	want := []EnsureEnvironmentOptions{{Name: "pdf-pymupdf", Requirements: []string{"pymupdf>=1.26,<3"}, Explicit: true}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %+v", calls)
	}
}

func TestExactlyOneSourceOfRequirements(t *testing.T) {
	for _, c := range []struct {
		id   string
		args EnvironmentArgs
	}{
		{"none", EnvironmentArgs{Name: "documents"}},
		{"both", EnvironmentArgs{Name: "documents", Extra: []string{"documents"}, Requirement: []string{"pypdf>=6,<7"}}},
	} {
		t.Run(c.id, func(t *testing.T) {
			ecProject(t)
			peSwap(t, &pmEnsureEnvironment, func(context.Context, EnsureEnvironmentOptions) (string, error) {
				t.Error("must not build")
				return "", nil
			})
			var code int
			var err error
			out, _ := ecCaptureFDs(t, func() { code, err = ecMain(c.args) })
			if code != 2 || err != nil || out != "" {
				t.Fatalf("main %d %v stdout %q", code, err, out)
			}
		})
	}
}

func TestAFailedBuildReportsOnStderrAndRestoresStdout(t *testing.T) {
	ecProject(t)
	peSwap(t, &pmEnsureEnvironment, func(context.Context, EnsureEnvironmentOptions) (string, error) {
		return "", NewInstallError("documents", "no network", "")
	})
	var code int
	var err error
	out, stderr := ecCaptureFDs(t, func() {
		code, err = ecMain(EnvironmentArgs{Name: "documents", Extra: []string{"documents"}})
		fmt.Fprintln(os.Stdout, "after")
	})
	if code != 1 || err != nil || out != "after\n" || !strings.Contains(stderr, "no network") {
		t.Fatalf("main %d %v stdout %q stderr %q", code, err, out, stderr)
	}
}
