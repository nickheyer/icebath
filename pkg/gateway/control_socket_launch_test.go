package gateway

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestLaunchSupervisorPrefersTheLaunchValueAndNeverAnswersNone(t *testing.T) {
	identity := func(pairs ...any) *pyjson.Object {
		obj := pyjson.NewObject()
		for i := 0; i+1 < len(pairs); i += 2 {
			obj.Set(pairs[i].(string), pairs[i+1])
		}
		return obj
	}
	for _, c := range []struct {
		identity *pyjson.Object
		want     string
	}{
		{identity("launch_supervisor", "desktop", "supervisor", "none"), "desktop"},
		{identity("launch_supervisor", "none", "supervisor", "systemd"), "systemd"},
		{identity("supervisor", "none"), ""},
		{identity("launch_supervisor", "", "supervisor", "launchd"), "launchd"},
		{identity("launch_supervisor", int64(3)), ""},
		{identity(), ""},
		{nil, ""},
	} {
		if got := LaunchSupervisor(c.identity); got != c.want {
			t.Errorf("LaunchSupervisor(%v) = %q want %q", c.identity, got, c.want)
		}
	}
}
