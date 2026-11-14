package commands

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

// Parsed logs invocation yields argparse dests, typed values and None for unset empty defaults
func TestCLINamespace(t *testing.T) {
	c := NewCLI()
	var got map[string]any
	c.Bind("logs", func(cmd *cobra.Command, args []string) error {
		got = c.Namespace(cmd, args)
		return nil
	})
	if err := c.Execute([]string{"logs", "errors", "-n", "100", "-f", "--since", "1h"}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"log_name": "errors", "lines": 100, "follow": true, "since": "1h", "level": nil, "session": nil, "component": nil, "command": "logs"}
	if _, ok := got["help"]; ok {
		t.Fatal("help flag leaked into the namespace")
	}
	for k, v := range want {
		if !reflect.DeepEqual(got[k], v) {
			t.Fatalf("%s = %#v, want %#v", k, got[k], v)
		}
	}
	c = NewCLI()
	c.Bind("logs", func(cmd *cobra.Command, args []string) error {
		got = c.Namespace(cmd, args)
		return nil
	})
	if err := c.Execute([]string{"logs"}); err != nil {
		t.Fatal(err)
	}
	if got["log_name"] != "agent" || got["lines"] != 50 || got["follow"] != false || got["since"] != nil {
		t.Fatalf("defaults = %#v", got)
	}
}
