package skills

import "github.com/dlclark/regexp2/v2"

// ThreatPatternByID returns the compiled regex behind one finding id so install scanners can locate its token
func ThreatPatternByID(id string) *regexp2.Regexp {
	for _, p := range compiledThreatPatterns {
		if p.id == id {
			return p.re
		}
	}
	return nil
}

// IsSuspiciousBinaryExtension reports whether a bundled file extension cannot be scanned
func IsSuspiciousBinaryExtension(ext string) bool {
	return suspiciousBinaryExtensions[ext]
}

// DetermineVerdict maps findings to safe, caution, or dangerous by their worst severity
func DetermineVerdict(findings []Finding) string {
	return determineVerdict(findings)
}
