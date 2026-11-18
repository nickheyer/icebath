package logging

import (
	"testing"
)

func TestTUIGatewayRecordsBelongToTheGUIComponentNotTheGatewayLog(t *testing.T) {
	gui, gateway := componentFilter{prefixes: ComponentPrefixes["gui"]}, componentFilter{prefixes: ComponentPrefixes["gateway"]}
	for name, want := range map[string][2]bool{
		"tuigateway": {true, false}, "tuiserver": {true, false},
		"gatewayrun": {false, true}, "gateway/platforms/telegram": {false, true},
	} {
		rec := &Record{Name: name}
		if gui.Filter(rec) != want[0] || gateway.Filter(rec) != want[1] {
			t.Fatalf("%s gui %v gateway %v, want %v", name, gui.Filter(rec), gateway.Filter(rec), want)
		}
	}
}
