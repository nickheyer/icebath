package gateway

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

func TestServiceStartDoesNotDecodeIrrelevantSupervisorOutput(t *testing.T) {
	service := &ExistingService{Backend: "systemd", StartArgv: []string{"/bin/sh", "-c", `printf '\377'`}}
	if err := StartExistingGatewayService(context.Background(), service, pythread.Monotonic()+5); err != nil {
		t.Fatal(err)
	}
}
