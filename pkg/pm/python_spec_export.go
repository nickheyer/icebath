//go:build !icebath_core && !icebath_noextras

package pm

import (
	"errors"
)

// Whether a PEP 440 specifier set admits version with prereleases allowed, invalid true for an unparsable set
func SpecifierSetAdmits(spec, version string) (admits, invalid bool, err error) {
	specs, err := parseSpecifierSet(spec)
	if err != nil {
		var bad *InvalidRequirementError
		if errors.As(err, &bad) {
			return false, true, nil
		}
		return false, false, err
	}
	admits, err = specifierSetContains(specs, version)
	return admits, false, err
}

// Full version of the pinned Python PM builds environments with, a RuntimeError when the lock pins none
func PinnedPythonFullVersion() (string, error) {
	version, ok := currentLockfile().Version("python")
	if !ok || version == "" {
		return "", noPythonPin(version)
	}
	return version, nil
}
