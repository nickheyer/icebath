package pm

// FeatureProbeError is a failed inventory, never read as an empty feature list
type FeatureProbeError struct{ Message string }

func (e *FeatureProbeError) Error() string { return e.Message }

// RuntimeError class callers catch it as
func (e *FeatureProbeError) Unwrap() error {
	return &PyError{Class: "RuntimeError", Message: e.Message}
}
