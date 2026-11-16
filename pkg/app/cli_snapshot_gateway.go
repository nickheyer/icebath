package app

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Restores a quick snapshot with the profile handed off the gateway, then rejoins this CLI's session
func (c *CLI) restoreGatewaySnapshot(store *gatewaycli.GatewaySessionStore, snapID string) (bool, error) {
	res, err := gatewaycli.RestoreSnapshotOffGateway(c.ctx, home.Dir(c.ctx), snapID, manualGatewayIdentity, func(h string) error {
		if err := store.CloseReader(); err != nil {
			return err
		}
		state.CloseAllUnder(h)
		store.Connection.Close(c.ctx)
		return nil
	})
	if res.Detached {
		if line := res.ReserveLine(); line != "" {
			cliLogger.Warn(fmt.Sprintf("Profile re-serve after snapshot restore: %s", pyrepr.Repr(res.Ack)))
			printLines("  " + line)
		}
		store.ForgetSessions()
		if rerr := c.rejoinAfterSnapshot(store); rerr != nil {
			cliLogger.Warn(fmt.Sprintf("Rejoin after snapshot restore failed: %v", rerr))
			printLines(fmt.Sprintf("  Reconnecting to the gateway failed: %s. Run /new or restart the CLI.", rerr.Error()))
		}
	}
	return res.Restored, err
}

// Moves the store to a fresh shared connection carrying the launch key and rejoins the CLI, a panic reported as its error
func (c *CLI) rejoinAfterSnapshot(store *gatewaycli.GatewaySessionStore) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = asError(r)
		}
	}()
	drained := store.Connection
	store.Connection = gatewaycli.SharedGatewayConnection()
	key, _ := drained.LaunchKey()
	_, typed := store.Connection.LaunchKey()
	store.Connection.SetLaunchKey(key, typed)
	return c.rejoinCLI(store)
}

// Reattaches the CLI to its session on the relaunched owner, minting a replacement when the snapshot predates it
func (c *CLI) rejoinCLI(store *gatewaycli.GatewaySessionStore) error {
	c.setAgent(nil)
	c.goalManager, c.loopManager, c.heartbeatManager = nil, nil, nil
	err := c.leaseHandedOver(c.activeSessionLease != nil, func() error {
		snapshot, err := store.Attach(c.ctx, c.sessionID, false)
		if err != nil {
			if !isGatewayClientError(err) || err.Error() != "not_found" {
				return err
			}
			launch := map[string]any{}
			for key, value := range gatewayLaunch(c) {
				if !gatewayRouteFlags[key] {
					launch[key] = value
				}
			}
			if _, err := store.Create(c.ctx, launch, "", false); err != nil {
				return err
			}
			snapshot = store.Current()
		}
		snap, _ := pyjson.Plain(snapshot).(map[string]any)
		c.uiMu.Lock()
		c.sessionID = pyrepr.Str(requireKey(snap, "stored_session_id"))
		c.uiMu.Unlock()
		rows, _ := snap["messages"].([]any)
		history := []map[string]any{}
		for _, row := range anyRows(rows) {
			if pyrepr.Truthy(row["archived"]) {
				continue
			}
			kept := copyMap(row)
			delete(kept, "row_id")
			history = append(history, kept)
		}
		c.conversationHistory = history
		bindGatewaySession(c, store.Connection)
		return nil
	})
	if err != nil {
		return err
	}
	c.writeTerminalBreadcrumb()
	return nil
}
