//go:build !linux && !darwin

package gateway

import "errors"

// Load averages are unobtainable here, the OSError os.getloadavg raises
func loadAverage1m() (float64, error) { return 0, errors.New("Load averages are unobtainable") }
