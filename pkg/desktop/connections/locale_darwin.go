package connections

import (
	"os/exec"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// Preferred UI languages from the global AppleLanguages default
func preferredUILanguages() []string {
	if _, err := exec.LookPath("defaults"); err != nil {
		return nil
	}
	out, err := backend.ExecText("defaults", []string{"read", "-g", "AppleLanguages"}, 5*time.Second)
	if err != nil {
		return nil
	}
	return parseAppleLanguages(out)
}
