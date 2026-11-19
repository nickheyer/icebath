package agent

import (
	"regexp"
)

var (
	logCallLineRE = regexp.MustCompile(`^(\S+ \S+) INFO \[(\S+)\] agent\.conversation_loop: API call #(\d+): model=(\S+) provider=\S+ ` +
		`in=(\d+) out=(\d+) total=\d+ latency=([\d.]+)s cache=(\d+)/(\d+)`)
	logCallWriteRE    = regexp.MustCompile(` write=(\d+)`)
	logCallIDRE       = regexp.MustCompile(` id=(\S+)`)
	logCallUpstreamRE = regexp.MustCompile(` upstream=(.+?)(?: [a-z_]+=|$)`)
)
