package selfupdate

import (
	"strings"
	"testing"
)

// The children an update starts skip the startup debts only a user launch reports
func TestUpdateInternalCommandNamesOnlyTheUpdateChildren(t *testing.T) {
	for argv, want := range map[string]bool{
		"update-completion /r/req.json /r/res.json": true, "update-completion /r/req.json /r/res.json --prepared": true,
		"update-finish /c /r": true, "source-completion --source /r --finish-update": true, "update-restart-recovery": true,
		"update-takeover /c /r": true, "venv-sync --check": true, "import-health": true,
		"update": false, "update --check": false, "gateway run": false, "chat -q update-finish": false, "": false,
	} {
		if got := UpdateInternalCommand(strings.Fields(argv)); got != want {
			t.Fatalf("%q got %v want %v", argv, got, want)
		}
	}
}
