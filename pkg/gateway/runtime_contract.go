package gateway

import "errors"

// RuntimeObservation is what a launcher observed about a profile's gateway owner
type RuntimeObservation struct {
	State            string
	InstalledService bool
	ServiceMayStart  bool
	UpdatePaused     bool
}

var startActions = map[string]string{
	"ready":        "attach",
	"starting":     "wait-owner",
	"draining":     "wait-owner",
	"incompatible": "reject-version",
	"inaccessible": "reject-access",
	"conflict":     "reject-conflict",
}

// Launch decision for an observation, the ValueError for a state outside the runtime contract
func NextStartAction(o RuntimeObservation) (string, error) {
	if o.UpdatePaused {
		return "wait-update", nil
	}
	if action, ok := startActions[o.State]; ok {
		return action, nil
	}
	if o.State != "absent" {
		return "", errors.New("unknown runtime state")
	}
	if o.ServiceMayStart {
		return "wait-service", nil
	}
	if o.InstalledService {
		return "start-service", nil
	}
	return "spawn-unmanaged", nil
}
