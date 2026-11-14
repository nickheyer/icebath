package commands

import (
	"testing"

	"github.com/spf13/cobra"
)

// Namespace of one invocation of a bound subcommand
func parsedNamespace(t *testing.T, sub string, argv []string) (map[string]any, error) {
	t.Helper()
	c := NewCLI()
	var got map[string]any
	c.Bind(sub, func(cmd *cobra.Command, args []string) error {
		got = c.Namespace(cmd, args)
		return nil
	})
	err := c.Execute(argv)
	return got, err
}

func TestLocalFlagParses(t *testing.T) {
	ns, err := parsedNamespace(t, "desktop", []string{"desktop", "--local"})
	if err != nil || ns["local"] != true {
		t.Fatalf("ns %v err %v", ns, err)
	}
}

func TestLocalFlagDefaultsOff(t *testing.T) {
	ns, err := parsedNamespace(t, "desktop", []string{"desktop"})
	if err != nil || ns["local"] != false {
		t.Fatalf("ns %v err %v", ns, err)
	}
}

func TestTheWebappCommandTakesOneActionAtATime(t *testing.T) {
	ns, err := parsedNamespace(t, "webapp", []string{"webapp", "--no-open", "--force-build"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"no_open": true, "status": false, "sign_out": false, "build_only": false, "force_build": true, "skip_build": false}
	for k, v := range want {
		if ns[k] != v {
			t.Fatalf("%s = %#v want %#v", k, ns[k], v)
		}
	}
	for _, clash := range [][]string{{"--status", "--sign-out"}, {"--status", "--build-only"}, {"--sign-out", "--build-only"}} {
		if _, err := parsedNamespace(t, "webapp", append([]string{"webapp"}, clash...)); err == nil {
			t.Fatalf("%v accepted", clash)
		}
	}
}
