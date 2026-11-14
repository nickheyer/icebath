package selfupdate

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestGatewayEventBudgetLeavesPreUpdateSnapshotHeadroom(t *testing.T) {
	if gateway.MaxGatewayEventBytes*32 > preUpdateSnapshotMaxFileSize {
		t.Fatalf("hosted room budget %d leaves no snapshot headroom below %d", gateway.MaxGatewayEventBytes, preUpdateSnapshotMaxFileSize)
	}
}
