package memory

import (
	"context"
	"io"
	"strings"
	"testing"
)

type fakeProviderCLIRun struct {
	name, dir, module, desc string
	handler                 bool
	argv                    []string
}

// Swaps the plugin host CLI seams for fakes recording every run
func fakeProviderCLIHost(t *testing.T, registerCLI, handler bool, describeErr error, rc int, dispatched bool) *[]fakeProviderCLIRun {
	runs := &[]fakeProviderCLIRun{}
	oldDescribe, oldRun := describeProviderCLI, runProviderCLI
	t.Cleanup(func() { describeProviderCLI, runProviderCLI = oldDescribe, oldRun })
	describeProviderCLI = func(ctx context.Context, name, dir, module string) (bool, bool, error) {
		*runs = append(*runs, fakeProviderCLIRun{name: name, dir: dir, module: module})
		return registerCLI, handler, describeErr
	}
	runProviderCLI = func(ctx context.Context, name, dir, module, desc string, withHandler bool, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, bool, error) {
		*runs = append(*runs, fakeProviderCLIRun{name: name, dir: dir, module: module, desc: desc, handler: withHandler, argv: argv})
		io.WriteString(stdout, "ran "+strings.Join(argv, " "))
		return rc, dispatched, nil
	}
	return runs
}
