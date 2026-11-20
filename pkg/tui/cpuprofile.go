package tui

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime/pprof"
	"time"

	"github.com/google/pprof/profile"
)

// Seconds between rewrites of the CPU profile file
const profileRotation = 2 * time.Second

// CPU profile merged into one file every rotation, so a killed process still leaves a whole profile
type cpuProfile struct {
	path   string
	buf    bytes.Buffer
	merged *profile.Profile
	err    error
	stop   chan struct{}
	done   chan struct{}
}

// Profiles the process into a file until the returned stop runs, nil when profiling does not start
func startCPUProfile(path string) func() {
	if path == "" {
		return nil
	}
	p := &cpuProfile{path: path, stop: make(chan struct{}), done: make(chan struct{})}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "icebath: cpu profile: "+err.Error())
		return nil
	}
	if err := pprof.StartCPUProfile(&p.buf); err != nil {
		fmt.Fprintln(os.Stderr, "icebath: cpu profile: "+err.Error())
		return nil
	}
	go p.run()
	return func() {
		close(p.stop)
		<-p.done
		if p.err != nil {
			fmt.Fprintln(os.Stderr, "icebath: cpu profile: "+p.err.Error())
		}
	}
}

// Rotates the profile on every tick and once more at the stop
func (p *cpuProfile) run() {
	defer close(p.done)
	tick := time.NewTicker(profileRotation)
	defer tick.Stop()
	for {
		select {
		case <-p.stop:
			p.rotate(false)
			return
		case <-tick.C:
			if !p.rotate(true) {
				return
			}
		}
	}
}

// Ends the running segment, merges it into the profile file and starts the next segment when asked
func (p *cpuProfile) rotate(again bool) bool {
	pprof.StopCPUProfile()
	seg := bytes.Clone(p.buf.Bytes())
	p.buf.Reset()
	if again {
		if err := pprof.StartCPUProfile(&p.buf); err != nil {
			p.err = err
			again = false
		}
	}
	if err := p.fold(seg); err != nil {
		p.err = errors.Join(p.err, err)
		if again {
			pprof.StopCPUProfile()
		}
		return false
	}
	return again
}

// Merges one finished segment into the profile and replaces the file with the result
func (p *cpuProfile) fold(seg []byte) error {
	part, err := profile.ParseData(seg)
	if err != nil {
		return err
	}
	if p.merged != nil {
		if part, err = profile.Merge([]*profile.Profile{p.merged, part}); err != nil {
			return err
		}
	}
	p.merged = part
	tmp := p.path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := part.Write(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}
