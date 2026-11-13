package gatewayrun

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
)

func TestSettledInfoOfATurnWithoutAnOwningSessionPublishesNothing(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	turn := &TurnRunner{runner: allocGatewayRunner(), ctx: newTurnContext()}
	publishSettledInfo(context.Background(), turn, &agent.AIAgent{})
	publishSettledInfo(context.Background(), turn, nil)
	if err := publishSettledInfoBody(context.Background(), turn, &agent.AIAgent{}); err == nil {
		t.Fatal("a turn without an owner reported no failure")
	}
}
