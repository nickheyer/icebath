package gateway

import (
	"context"
	"strings"
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// One attested text input of three bytes bound to its source event
func attestedOneInput(extra ...any) *jsonutil.Object {
	manifest := []any{map[string]any{"attachment_id": "att_0123456789abcdef0123456789abcdef", "kind": "file", "mime": "text/plain", "name": "a.txt", "size": 3, "event_id": "evt-1"}}
	return jsonutil.ObjectOf(append([]any{"attachments", manifest}, extra...)...)
}

func TestHostedAttachmentDataWithoutInputsTransfersNothing(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, attested := range []*jsonutil.Object{jsonutil.NewObject(), jsonutil.ObjectOf("attachments", []any{})} {
		got, err := hostedAttachmentData(context.Background(), jsonutil.NewObject(), attested, map[string]any{})
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("got %v, %v", got, err)
		}
	}
}

func TestHostedAttachmentDataRefusesDigestsThatDoNotCoverTheManifest(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, attested := range []*jsonutil.Object{attestedOneInput(), attestedOneInput("attachment_digests", []any{}), attestedOneInput("attachment_digests", "ab")} {
		if _, err := hostedAttachmentData(context.Background(), jsonutil.NewObject(), attested, map[string]any{}); !isRuntimeRefusal(err, "permission_denied") {
			t.Fatalf("got %v", err)
		}
	}
}

func TestHostedAttachmentDataNeedsTheAttestedPromptBeforeItAsksForAChunk(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	_, err := hostedAttachmentData(context.Background(), jsonutil.NewObject(), attestedOneInput("attachment_digests", []any{"ab"}), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("a missing prompt gave %v", err)
	}
	_, err = hostedAttachmentData(context.Background(), jsonutil.NewObject(), attestedOneInput("attachment_digests", []any{"ab"}, "prompt", 7), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "prompt is a number, expected text") {
		t.Fatalf("a prompt that is no text gave %v", err)
	}
}
