package gateway

import "sync"

// Runner whose session API is mounted in this process, the gateway/run_api.py _ACTIVE_RUNNER
var activeAPIRunner struct {
	sync.Mutex
	runner AuthorityRunner
}

// Records the runner the session API mount serves, nil when the mount closes
func SetActiveAPIRunner(runner AuthorityRunner) {
	activeAPIRunner.Lock()
	defer activeAPIRunner.Unlock()
	activeAPIRunner.runner = runner
}

// Runner the session API mount serves, nil when none is mounted
func ActiveAPIRunner() AuthorityRunner {
	activeAPIRunner.Lock()
	defer activeAPIRunner.Unlock()
	return activeAPIRunner.runner
}
