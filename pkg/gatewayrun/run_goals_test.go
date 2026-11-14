package gatewayrun

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestStreamedAlreadySentNoneRecoversTextForHooks(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	event := gateway.NewMessageEvent("wakeup")
	event.SetStreamedFinalResponse("CI is green.\nLOOP_COMPLETE")
	if got := finalTextForPostTurnHooks(nil, event); got != "CI is green.\nLOOP_COMPLETE" {
		t.Fatalf("streamed text not recovered: %q", got)
	}
	if got := finalTextForPostTurnHooks(nil, gateway.NewMessageEvent("x")); got != "" {
		t.Fatalf("unstreamed event yielded %q", got)
	}
	if got := finalTextForPostTurnHooks(map[string]any{"final_response": "from dict"}, event); got != "from dict" {
		t.Fatalf("dict result lost: %q", got)
	}
}
