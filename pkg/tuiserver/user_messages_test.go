package tuiserver

import (
	"errors"
	"strings"
	"testing"
)

func TestAuthLockTimeoutContentionGetsTheWaitCopy(t *testing.T) {
	for name, text := range map[string]string{
		"auth-store-with-holder": "Timed out waiting for auth store lock (/home/u/.icebath/profiles/coder/auth.lock); another icebath process (pid 4242) probably still holds it",
		"auth-store-no-holder":   "Timed out waiting for auth store lock (/home/u/.icebath/profiles/coder/auth.lock)",
		"shared-nous-store":      "Timed out waiting for shared Nous auth lock (/home/u/.icebath/shared/nous.lock)",
	} {
		t.Run(name, func(t *testing.T) {
			message := agentInitFailedMessage(errors.New(text))
			if strings.Contains(message, "/model") || strings.Contains(message, "icebath setup") {
				t.Fatalf("lock contention kept the setup hints: %q", message)
			}
			if !strings.Contains(message, "lock") || !strings.Contains(message, "dashboard") {
				t.Fatalf("missing the wait copy: %q", message)
			}
		})
	}
}

func TestGenericTimeoutKeepsTheModelSetupHints(t *testing.T) {
	message := agentInitFailedMessage(errors.New("connect timed out"))
	if !strings.Contains(message, "/model") || !strings.Contains(message, "icebath setup") {
		t.Fatalf("message = %q", message)
	}
}

func TestOtherInitFailuresKeepTheModelSetupHints(t *testing.T) {
	message := agentInitFailedMessage(errors.New("provider bootstrap failed"))
	if !strings.Contains(message, "/model") || !strings.Contains(message, "icebath setup") {
		t.Fatalf("message = %q", message)
	}
}
