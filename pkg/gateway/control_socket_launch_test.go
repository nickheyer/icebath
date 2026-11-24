package gateway

import (
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestLaunchSupervisorPrefersTheLaunchValueAndNeverAnswersNone(t *testing.T) {
	identity := func(pairs ...any) *jsonutil.Object {
		obj := jsonutil.NewObject()
		for i := 0; i+1 < len(pairs); i += 2 {
			obj.Set(pairs[i].(string), pairs[i+1])
		}
		return obj
	}
	for _, c := range []struct {
		identity *jsonutil.Object
		want     string
	}{
		{identity("launch_supervisor", "external", "supervisor", "none"), "external"},
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
