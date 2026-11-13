package backend

import "sync"

// InstallIDEntry is a cached installation id of a connection and when it was read in unix milliseconds
type InstallIDEntry struct {
	ID string
	TS int64
}

// ConnectionCaches hold the per connection caches a removed or repointed connection must drop
type ConnectionCaches struct {
	mu                 sync.Mutex
	sshRoster          map[string][]string
	sshInventoryTried  map[string]int64
	installIDs         map[string]InstallIDEntry
	rosterSourceErrors map[string]string
}

// Caches every desktop process shares, keyed by connection id
var SharedConnectionCaches = &ConnectionCaches{sshRoster: map[string][]string{}, sshInventoryTried: map[string]int64{},
	installIDs: map[string]InstallIDEntry{}, rosterSourceErrors: map[string]string{}}

// Cached SSH profile roster of a connection
func (c *ConnectionCaches) SSHRoster(id string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.sshRoster[id]
	return v, ok
}

// Caches the SSH profile roster of a connection
func (c *ConnectionCaches) SetSSHRoster(id string, roster []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sshRoster[id] = roster
}

// Drops the cached SSH profile roster of a connection
func (c *ConnectionCaches) DeleteSSHRoster(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sshRoster, id)
}

// When the SSH inventory of a connection was last attempted in unix milliseconds
func (c *ConnectionCaches) SSHInventoryAttemptedAt(id string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.sshInventoryTried[id]
	return v, ok
}

// Records an SSH inventory attempt of a connection
func (c *ConnectionCaches) SetSSHInventoryAttemptedAt(id string, at int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sshInventoryTried[id] = at
}

// Drops the SSH inventory attempt time of a connection
func (c *ConnectionCaches) DeleteSSHInventoryAttemptedAt(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sshInventoryTried, id)
}

// Cached installation id of a connection
func (c *ConnectionCaches) InstallID(id string) (InstallIDEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.installIDs[id]
	return v, ok
}

// Caches the installation id of a connection
func (c *ConnectionCaches) SetInstallID(id string, entry InstallIDEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.installIDs[id] = entry
}

// Last roster source error of a connection
func (c *ConnectionCaches) RosterSourceError(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.rosterSourceErrors[id]
	return v, ok
}

// Records the roster source error of a connection
func (c *ConnectionCaches) SetRosterSourceError(id, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rosterSourceErrors[id] = message
}

// Drops the roster source error of a connection
func (c *ConnectionCaches) DeleteRosterSourceError(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.rosterSourceErrors, id)
}

// Drops every cache entry of a connection, nothing for an empty id
func (c *ConnectionCaches) Evict(connectionID string) {
	if connectionID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sshRoster, connectionID)
	delete(c.sshInventoryTried, connectionID)
	delete(c.installIDs, connectionID)
	delete(c.rosterSourceErrors, connectionID)
}
