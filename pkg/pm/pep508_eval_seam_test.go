package pm

// EvaluateMarker evaluates a PEP 508 marker string in process, the environment over defaults like Marker.evaluate
func EvaluateMarker(marker string, defaults, environment map[string]string) (bool, error) {
	markers, err := parseMarker(marker)
	if err != nil {
		return false, err
	}
	return evaluateMarker(markers, defaults, environment)
}
