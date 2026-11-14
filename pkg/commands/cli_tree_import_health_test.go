package commands

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

func TestImportHealthEntryIsHiddenAndTakesRawArguments(t *testing.T) {
	c := NewCLI()
	if cmd := c.find("import-health"); cmd == nil || !cmd.Hidden {
		t.Fatal("import-health is not a hidden command")
	}
	var got []string
	c.Bind("import-health", func(cmd *cobra.Command, args []string) error { got = args; return nil })
	argv := []string{"import-health", "__ICEBATH_IMPORT_HEALTH_0__", "False", "-h", "--json"}
	if err := c.Execute(argv); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, argv[1:]) {
		t.Fatalf("import-health received %v", got)
	}
}
