package tools

import (
	"reflect"
	"testing"
)

func TestServiceDefinitionHomesExportedFindsEverySyntax(t *testing.T) {
	definition := "Environment=\"ICEBATH_HOME=/srv/a\"\n<key>ICEBATH_HOME</key>\n  <string>/srv/b</string>\nItem(\"ICEBATH_HOME\") = \"C:\\c\"\n"
	want := []string{"/srv/a", "/srv/b", "C:\\c"}
	if got := ServiceDefinitionHomes(definition); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}
