package pm

import (
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
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
		return "", &PyError{Class: "RuntimeError", Message: fmt.Sprintf("the lock pins no python version: %s", pyrepr.StrRepr(version))}
	}
	return version, nil
}
