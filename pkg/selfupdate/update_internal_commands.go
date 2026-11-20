//go:build !icebath_core && !icebath_noextras

package selfupdate

import "github.com/nickheyer/icebath/pkg/pm"

// Hidden verbs an update runs in its own children, scripts the reference starts outside its CLI startup
var updateInternalCommands = map[string]bool{
	UpdateCompletionEntry: true, UpdateFinishEntry: true, SourceCompletionEntry: true, UpdateRestartRecoveryEntry: true,
	"update-takeover": true, "venv-sync": true, pm.ImportHealthEntry: true,
}

// Reports an argv the update spawned for one of its children, the hidden verb leading it
func UpdateInternalCommand(argv []string) bool {
	return len(argv) > 0 && updateInternalCommands[argv[0]]
}
