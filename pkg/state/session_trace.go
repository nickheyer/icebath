package state

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
)

// Installs fn as the statement trace of the writer and of every read connection handed out, nil removing it
func (d *SessionDB) SetTraceCallback(fn func(sql string)) error {
	d.traceMu.Lock()
	d.traceFn = fn
	previous := d.tracedConns
	d.tracedConns = map[SQLiteConn]bool{}
	d.traceMu.Unlock()
	targets := make([]SQLiteConn, 0, len(previous)+1)
	for conn := range previous {
		targets = append(targets, conn)
	}
	d.mu.Lock()
	if d.conn != nil {
		targets = append(targets, d.conn)
	}
	d.mu.Unlock()
	for _, conn := range targets {
		if fn == nil {
			raw, ok := rawConnOf(conn)
			if !ok {
				return fmt.Errorf("connection %T exposes no driver connection for a statement trace", conn)
			}
			if err := pysqlite.SetTraceCallback(raw, nil); err != nil {
				return err
			}
			continue
		}
		if err := d.applyTrace(conn); err != nil {
			return err
		}
	}
	return nil
}

// Applies the current trace function to a connection the first time it is handed out
func (c *sessionCore) applyTrace(conn SQLiteConn) error {
	c.traceMu.Lock()
	fn := c.traceFn
	if fn == nil || c.tracedConns[conn] {
		c.traceMu.Unlock()
		return nil
	}
	c.tracedConns[conn] = true
	c.traceMu.Unlock()
	raw, ok := rawConnOf(conn)
	if !ok {
		return fmt.Errorf("connection %T exposes no driver connection for a statement trace", conn)
	}
	return pysqlite.SetTraceCallback(raw, fn)
}

// Drops a closing connection from the traced set
func (c *sessionCore) forgetTraced(conn SQLiteConn) {
	c.traceMu.Lock()
	delete(c.tracedConns, conn)
	c.traceMu.Unlock()
}
