package install

import (
	"regexp"
)

var canaryReleaseTagRE = regexp.MustCompile(`^v\d+\.\d+\.\d+\+canary\.20\d{6}T\d{6}Z$`)
