//go:build windows

package agent

import (
	"os"
	"time"
)

// Ends this worker after the orphan grace, Windows having no process group to kill
func dieWithOwner() {
	time.AfterFunc(orphanGraceSeconds*time.Second, func() { os.Exit(1) })
}
