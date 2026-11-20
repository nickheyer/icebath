package profiles

// A profile home whose gateway.lock another holder owns, the gateway OwnershipConflict seen from profiles
type GatewayOwnershipConflict struct{ Msg string }

func (e *GatewayOwnershipConflict) Error() string { return e.Msg }

// Gateway runtime facts pkg/gateway installs in init, nil in a binary that does not link it
var GatewayStatusHooks struct {
	ProfileRunning         func(profileDir string) bool
	ServesNamedProfile     func(name string) bool
	NotifyProfilesChanged  func(name string)
	RecordedServedProfiles func() ([]string, bool, error)
	ExclusiveMaintenance   func(dirs []string) (func(), error)
	PidExists              func(pid int) bool
	ProcessStartTime       func(pid int) (int64, bool)
	TerminatePid           func(pid int, force bool, expectedStart *int64) error
	RecordedHomeConflicts  func(record map[string]any, expectedHome string) bool
	Identify               func(root string) (any, error)
	MigrateProfileIdentity func(root, oldName, newName string) (any, error)
	PurgeProfileIdentity   func(root, name string) (any, error)
}
