package gateway

import (
	"context"
	"testing"

	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

func TestServiceStartDoesNotDecodeIrrelevantSupervisorOutput(t *testing.T) {
	service := &ExistingService{Backend: "systemd", StartArgv: []string{"/bin/sh", "-c", `printf '\377'`}}
	if err := StartExistingGatewayService(context.Background(), service, timeutil.Monotonic()+5); err != nil {
		t.Fatal(err)
	}
}
