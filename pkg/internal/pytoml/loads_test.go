package pytoml

import (
	"errors"
	"fmt"
	"testing"
)

func TestLoadsAcceptsAndRefusesLikeTomllib(t *testing.T) {
	doc, err := Loads("model = \"x\"\n[mcp_servers]\ngbrain = { command = \"g\" }\n[mcp_servers.other]\ncommand = \"o\"\n")
	if err != nil {
		t.Fatal(err)
	}
	servers := doc["mcp_servers"].(map[string]any)
	if servers["gbrain"].(map[string]any)["command"] != "g" || servers["other"].(map[string]any)["command"] != "o" {
		t.Fatalf("parsed %#v", doc)
	}
	for _, bad := range []string{
		"[a]\nx = 1\n[a]\ny = 2\n",
		"[mcp_servers]\ngbrain = { command = \"g\" }\n[mcp_servers.gbrain]\ncommand = \"h\"\n",
		"x = 1\nx = 2\n",
		"[plugins.\"a@b\"]\nallowed = [\n",
		"[features]\ndefault_permissions = \":workspace\"\n[features.default_permissions]\nx = 1\n",
		"k = \"a\x01b\"\n",
		"é = 1\n",
	} {
		_, err := Loads(bad)
		var de *DecodeError
		if !errors.As(err, &de) || de.Msg == "" {
			t.Fatalf("%q parsed, want DecodeError, got %v", bad, err)
		}
	}
	_, err = Loads("a = 1\nb = [\n")
	if de := err.(*DecodeError); de.Line != 2 || de.Error() != fmt.Sprintf("%s (at line 2, column %d)", de.Msg, de.Col) {
		t.Fatalf("position %#v %q", de, de.Error())
	}
	_, err = Loads("x = 1\nx = 2\n")
	if de := err.(*DecodeError); de.Error() != "key x is already defined" {
		t.Fatalf("duplicate %#v %q", de, de.Error())
	}
}
