package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/containerinit"
)

// Runs the container init, the supervisor or the s6 verb its name or first argument selects
func main() {
	if code, ok := containerinit.Dispatch(os.Args); ok {
		os.Exit(code)
	}
	fmt.Fprintf(os.Stderr, "%s: usage: run as init, tini, s6-svscan, s6-svc, s6-svstat, s6-svwait, s6-svscanctl, "+
		"s6-setuidgid, with-contenv or s6-log, or with container-init or container-health as the first argument\n",
		filepath.Base(os.Args[0]))
	os.Exit(100)
}
