package gatewaycli

import (
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Whether a host control answer confirms name under key without an error
func AckConfirmed(answer *jsonutil.Object, key, name string) bool {
	return ackConfirmed(answer, key, name)
}

// Why a host control answer did not confirm the operation
func AckFailure(answer *jsonutil.Object) string { return ackFailure(answer) }
