package gateway

import "fmt"

var ensureExitCodes = map[string]int{"ready": 0, "incompatible": 3, "draining": 6, "inaccessible": 7, "conflict": 7, "starting": 5, "absent": 5}

// Process exit code of an ensure verdict, the KeyError for a state outside the contract
func EnsureExitCode(result ObservedEndpoint) (int, error) {
	switch result.ReasonCode {
	case "deadline":
		return 5, nil
	case "authorization", "profile_mismatch", "gateway_elevated", "gateway_other_session":
		return 4, nil
	}
	code, ok := ensureExitCodes[result.State]
	if !ok {
		return 0, fmt.Errorf("'%s'", result.State)
	}
	return code, nil
}
