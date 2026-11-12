package tools

import "path/filepath"

// Confines the install probe to a marker under dir and the active root, the returned function restoring the machine
func ConfineInstallProbeForTests(dir string) func() {
	prev := machinePlaces
	places := InstallPlaces{Markers: []string{filepath.Join(dir, "machine-marker")}, Sources: []InstallSource{installActiveRoot}}
	machinePlaces = func() InstallPlaces { return places }
	InstallStateProbeClear()
	return func() {
		machinePlaces = prev
		InstallStateProbeClear()
	}
}
