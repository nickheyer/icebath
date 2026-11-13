package connections

// ApplyAtomicallyOptions name the stores an apply writes, the live change it runs and the preflight that gates both
type ApplyAtomicallyOptions[C, R any] struct {
	Apply            func() error
	NextConfig       C
	NextRegistry     R
	Preflight        func() error
	PreviousConfig   C
	PreviousRegistry R
	WriteConfig      func(C) error
	WriteRegistry    func(R) error
}

// Runs the preflight, writes both stores and applies, restoring both previous stores when a write or the apply fails
func ApplyConnectionConfigAtomically[C, R any](o ApplyAtomicallyOptions[C, R]) error {
	if o.Preflight != nil {
		if err := o.Preflight(); err != nil {
			return err
		}
	}
	err := o.WriteConfig(o.NextConfig)
	if err == nil {
		err = o.WriteRegistry(o.NextRegistry)
	}
	if err == nil {
		err = o.Apply()
	}
	if err != nil {
		if o.WriteConfig(o.PreviousConfig) == nil {
			_ = o.WriteRegistry(o.PreviousRegistry)
		}
		return err
	}
	return nil
}
