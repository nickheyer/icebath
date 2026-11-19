package proc

import (
	"regexp"
)

var numThreadsRE = regexp.MustCompile(`Threads:\t(\d+)`)
