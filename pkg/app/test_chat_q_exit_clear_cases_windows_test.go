package app

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
	"golang.org/x/sys/windows"
)

func TestClearFallbackWindowsRunsClsWithHiddenConsole(t *testing.T) {
	calls := recordClearRuns(t)
	installFallbackConsole(t)
	bareCLI().wipeConsoleOnExit()
	if attr := tools.WindowsHideAttr(); attr == nil || attr.CreationFlags != windows.CREATE_NO_WINDOW {
		t.Fatalf("hide flags %+v", attr)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls %d", len(*calls))
	}
	cmd := (*calls)[0]
	if !reflect.DeepEqual(cmd.Args, []string{"cmd", "/c", "cls"}) {
		t.Fatalf("argv %v", cmd.Args)
	}
	if cmd.Stdin != nil {
		t.Fatalf("stdin %v, want the null device", cmd.Stdin)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags != windows.CREATE_NO_WINDOW {
		t.Fatalf("creation flags %+v", cmd.SysProcAttr)
	}
}
