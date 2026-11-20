//go:build !icebath_core && !icebath_noextras

package media

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Pool backed account double, every method the secret resolver never reaches left to the embedded nil interface
type voicePoolAccount struct {
	pool map[string]string
}

func (a *voicePoolAccount) PoolAPIKey(_ *tools.Env, id string) string        { return a.pool[id] }
func (a *voicePoolAccount) SourceSuppressed(*tools.Env, string, string) bool { return false }

func TestToolWiringTranscriptionToolsDelegates(t *testing.T) {
	withVoiceHome(t, "")
	t.Setenv("GROQ_API_KEY", "")
	swapVoice[tools.PoolKeySource](t, &tools.PoolKeys, &voicePoolAccount{pool: map[string]string{"groq": "stt-pool-key"}})
	if got := resolveSTTProviderKey(context.Background(), "GROQ_API_KEY", "groq"); got != "stt-pool-key" {
		t.Fatalf("got %q", got)
	}
}
