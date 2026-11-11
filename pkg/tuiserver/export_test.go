package tuiserver

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Registered handler of a method for the external test package
var LookupMethod = lookupMethod

// Points the launch store at db for an external test case
func UseSharedDB(t *testing.T, db *state.DB) { useSharedDB(t, db) }
