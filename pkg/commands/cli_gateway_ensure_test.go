package commands

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

func TestGatewayEnsureParseErrorsPrintTheProtocolLineOnStdout(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, argv := range [][]string{
		{"gateway", "ensure", "extra"},
		{"gateway", "ensure", "--bogus"},
		{"gateway", "ensure", "--timeout"},
	} {
		tree := NewCLI()
		tree.Bind("gateway ensure", func(*cobra.Command, []string) error {
			t.Fatalf("%v reached the handler", argv)
			return nil
		})
		var out, errOut bytes.Buffer
		tree.Root.SetOut(&out)
		tree.Root.SetErr(&errOut)
		err := tree.Execute(argv)
		var exit *ExitError
		if !errors.As(err, &exit) || exit.Code != 2 || exit.Msg != "gateway ensure: invalid invocation" {
			t.Fatalf("%v error %v", argv, err)
		}
		if out.String() != `{"endpoint":null,"reason_code":"invalid_invocation","state":"inaccessible"}`+"\n" || errOut.Len() != 0 {
			t.Fatalf("%v stdout %q stderr %q", argv, out.String(), errOut.String())
		}
	}
}
