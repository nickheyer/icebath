package selfupdate

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane/remoteplane"
)

// The doctor reports the remote backend while the plane serves and fails the check with an issue when it cannot
func TestDoctorReportsBackendStatus(t *testing.T) {
	p := remoteplane.Setup(t)
	ctx := context.Background()
	var out bytes.Buffer
	d := &Doctor{W: &out, Home: p.Home, DisplayHome: p.Home}
	backend, err := config.GetConfigBackend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := doctorCheckConfigBackend(ctx, d, backend, &Finding{}); !ok || err != nil {
		t.Fatalf("healthy plane %v %v", ok, err)
	}
	if !strings.Contains(out.String(), "Remote Config") {
		t.Fatalf("output %q", out.String())
	}
	p.Mu.Lock()
	p.FailStatus = 503
	p.Mu.Unlock()
	remote.ResetForTests()
	backend, err = config.GetConfigBackend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f2 := &Finding{}
	if ok, err := doctorCheckConfigBackend(ctx, d, backend, f2); ok || err != nil || len(f2.Issues) == 0 {
		t.Fatalf("outage %v %v issues %v", ok, err, f2.Issues)
	}
}
