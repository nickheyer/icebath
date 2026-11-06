package agent

import (
	"fmt"
	"strings"
	"syscall"
)

// deadConnectionAgent is the agent surface the dead connection sweep reads and repairs
type deadConnectionAgent interface {
	primaryClient() any
	cachedRequestClient() any
	replacePrimaryOpenAIClient(reason string)
	closeCachedRequestOpenAIClient(reason string)
}

func (a *AIAgent) primaryClient() any { return a.client }

// Client held by the per request client cache, nil when the cache is empty
func (a *AIAgent) cachedRequestClient() any {
	if a.requestClientCache == nil {
		return nil
	}
	return a.requestClientCache["client"]
}

// Number of pool sockets of the client that reached EOF
func countDeadPoolSockets(client any) (int, error) {
	n := 0
	for _, sock := range iterPoolSockets(client) {
		dead, err := socketIsDead(sock)
		if err != nil {
			return 0, err
		}
		if dead {
			n++
		}
	}
	return n, nil
}

// Rebuilds a primary client or retires a cached request client whose pool holds half closed sockets
func cleanupDeadConnections(a deadConnectionAgent) bool {
	type entry struct {
		label  string
		client any
		dead   int
	}
	primary := a.primaryClient()
	request := a.cachedRequestClient()
	var clients []entry
	if primary != nil {
		clients = append(clients, entry{label: "primary", client: primary})
	}
	if request != nil && request != primary {
		clients = append(clients, entry{label: "request", client: request})
	}
	if len(clients) == 0 {
		return false
	}
	var dead []entry
	for _, c := range clients {
		func() {
			defer func() {
				if r := recover(); r != nil {
					runAgentLog().Debug(fmt.Sprintf("Dead connection check error for %s client: %v", c.label, r))
				}
			}()
			n, err := countDeadPoolSockets(c.client)
			if err != nil {
				runAgentLog().Debug(fmt.Sprintf("Dead connection check error for %s client: %v", c.label, err))
				return
			}
			if n > 0 {
				c.dead = n
				dead = append(dead, c)
			}
		}()
	}
	if len(dead) == 0 {
		return false
	}
	total := 0
	labels := make([]string, 0, len(dead))
	for _, d := range dead {
		total += d.dead
		labels = append(labels, d.label)
	}
	runAgentLog().Warn(fmt.Sprintf("Found %d dead connection(s) in %s client pool(s) — cleaning up", total, strings.Join(labels, ", ")))
	for _, d := range dead {
		func() {
			defer func() {
				if r := recover(); r != nil {
					runAgentLog().Debug(fmt.Sprintf("Dead connection cleanup error for %s client: %v", d.label, r))
				}
			}()
			if d.label == "primary" {
				a.replacePrimaryOpenAIClient("dead_connection_cleanup")
			} else {
				a.closeCachedRequestOpenAIClient("dead_connection_cleanup")
			}
		}()
	}
	return true
}

// Probes a socket with a non blocking peek, dead on EOF or an OS error, healthy when no data is pending
func socketIsDead(sock syscall.Conn) (bool, error) {
	rc, err := sock.SyscallConn()
	if err != nil {
		return true, nil
	}
	var dead bool
	var probeErr error
	ctrlErr := rc.Control(func(fd uintptr) { dead, probeErr = peekSocketDead(fd) })
	if ctrlErr != nil {
		return true, nil
	}
	return dead, probeErr
}
