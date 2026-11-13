package system

// KeepAwakeType is the powerSaveBlocker kind, app suspension keeps the system up while the display may sleep
type KeepAwakeType string

const (
	PreventAppSuspension KeepAwakeType = "prevent-app-suspension"
	PreventDisplaySleep  KeepAwakeType = "prevent-display-sleep"
)

// PowerSaveBlocker is the OS sleep blocker, ids identify each held request
type PowerSaveBlocker interface {
	Start(kind KeepAwakeType) int
	Stop(id int)
	IsStarted(id int) bool
}

// KeepAwake holds at most one blocker of one kind
type KeepAwake struct {
	blocker PowerSaveBlocker
	kind    KeepAwakeType
	id      *int
}

// Keep awake toggle over blocker, app suspension when kind is empty
func NewKeepAwake(blocker PowerSaveBlocker, kind KeepAwakeType) *KeepAwake {
	if kind == "" {
		kind = PreventAppSuspension
	}
	return &KeepAwake{blocker: blocker, kind: kind}
}

// True while the held blocker is still started
func (k *KeepAwake) IsActive() bool {
	return k.id != nil && k.blocker.IsStarted(*k.id)
}

// Starts or stops the blocker idempotently and answers whether it is held
func (k *KeepAwake) Set(on bool) bool {
	if on && !k.IsActive() {
		id := k.blocker.Start(k.kind)
		k.id = &id
	} else if !on && k.id != nil {
		if k.blocker.IsStarted(*k.id) {
			k.blocker.Stop(*k.id)
		}
		k.id = nil
	}
	return k.IsActive()
}

// KeepAwakeMode is the persisted keep awake setting
type KeepAwakeMode string

const (
	KeepAwakeAlways       KeepAwakeMode = "always"
	KeepAwakeOff          KeepAwakeMode = "off"
	KeepAwakeWhileWorking KeepAwakeMode = "while-working"
)

// DefaultKeepAwakeMode holds the machine awake only while a turn runs
const DefaultKeepAwakeMode = KeepAwakeWhileWorking

// Mode of a renderer value, booleans read as always and off, nil when it is no mode
func ParseKeepAwakeMode(value any) *KeepAwakeMode {
	var mode KeepAwakeMode
	switch v := value.(type) {
	case bool:
		mode = KeepAwakeOff
		if v {
			mode = KeepAwakeAlways
		}
	case string:
		switch KeepAwakeMode(v) {
		case KeepAwakeOff, KeepAwakeWhileWorking, KeepAwakeAlways:
			mode = KeepAwakeMode(v)
		default:
			return nil
		}
	default:
		return nil
	}
	return &mode
}

// Mode of the persisted file, the legacy on flag second, the default otherwise
func ReadKeepAwakeMode(persisted any) KeepAwakeMode {
	record, ok := persisted.(map[string]any)
	if !ok || record == nil {
		return DefaultKeepAwakeMode
	}
	if mode := ParseKeepAwakeMode(record["mode"]); mode != nil {
		return *mode
	}
	if mode := ParseKeepAwakeMode(record["on"]); mode != nil {
		return *mode
	}
	return DefaultKeepAwakeMode
}

// Whether the blocker should be held for mode with a turn in flight or not
func KeepAwakeWanted(mode KeepAwakeMode, working bool) bool {
	return mode == KeepAwakeAlways || (mode == KeepAwakeWhileWorking && working)
}
