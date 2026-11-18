package logging

import (
	"testing"
)

func TestTUIGatewayRecordsBelongToTheGUIComponentNotTheGatewayLog(t *testing.T) {
	gui, gateway := componentFilter{prefixes: ComponentPrefixes["gui"]}, componentFilter{prefixes: ComponentPrefixes["gateway"]}
	for name, want := range map[string][2]bool{
		"tui_gateway.ws": {true, false}, "tui_gateway.server": {true, false},
		"gateway.run": {false, true}, "plugins.platforms.telegram.adapter": {false, true},
	} {
		rec := &Record{Name: name}
		if gui.Filter(rec) != want[0] || gateway.Filter(rec) != want[1] {
			t.Fatalf("%s gui %v gateway %v, want %v", name, gui.Filter(rec), gateway.Filter(rec), want)
		}
	}
}
