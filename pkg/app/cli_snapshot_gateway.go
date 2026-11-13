package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/state"
)

// Hands the profile off to maintenance, true when this restore parked a satellite and must serve it again
func pauseSnapshotOwner(h, controlHome string, identity *pyjson.Object) (bool, error) {
	satellite := controlHome != h
	ours, accepted := false, false
	name := filepath.Base(h)
	if satellite {
		marker := profiles.ParkedMarkerPath(h)
		if f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666); err == nil {
			f.Close()
			ours = true
		} else if !errors.Is(err, os.ErrExist) {
			return false, err
		}
		ack := gateway.RequestUnserveProfile(controlHome, name)
		accepted = gatewaycli.AckConfirmed(ack, "unserved", name) ||
			(ack != nil && pyrepr.Truthy(ack.GetOr("pending", nil)) && !pyrepr.Truthy(ack.GetOr("error", nil)))
		if ours && !accepted {
			_ = os.Remove(marker)
		}
	} else {
		accepted = pauseAcked(gateway.PauseGatewayForUpdate(controlHome, 8*time.Second), identity)
	}
	if !accepted {
		retry := "Stop the gateway and retry."
		if satellite {
			retry = fmt.Sprintf("Park the profile with `icebath -p %s gateway stop` and retry.", name)
		}
		return ours, &gateway.GatewayClientError{Msg: "Gateway refused the snapshot maintenance handoff; nothing was restored. " + retry}
	}
	return ours, nil
}

// Restores a quick snapshot with the profile handed off the gateway, then rejoins this CLI's session
func (c *CLI) restoreGatewaySnapshot(store *gatewaycli.GatewaySessionStore, snapID string) (restored bool, err error) {
	h, err := filepath.Abs(home.Dir(c.ctx))
	if err != nil {
		return false, err
	}
	if resolved, rerr := filepath.EvalSymlinks(h); rerr == nil {
		h = resolved
	}
	observed := gateway.DiscoverGatewayEndpoint(c.ctx, h, 2*time.Second)
	if observed.State != "ready" || observed.Endpoint == nil {
		return false, &gateway.GatewayClientError{Msg: "Local gateway is not ready for snapshot maintenance. Reconnect and retry."}
	}
	controlHome, err := filepath.Abs(gateway.ControlHomeFor(h, observed.Endpoint))
	if err != nil {
		return false, err
	}
	if resolved, rerr := filepath.EvalSymlinks(controlHome); rerr == nil {
		controlHome = resolved
	}
	var identity *pyjson.Object
	if controlHome == h {
		identity, err = manualGatewayIdentity(h)
		if err != nil {
			return false, &gateway.GatewayClientError{Msg: fmt.Sprintf("Nothing was restored: %s.", err.Error())}
		}
	}
	if err := store.CloseReader(); err != nil {
		return false, err
	}
	state.CloseAllUnder(h)
	store.Connection.Close(c.ctx)
	unserved := false
	defer func() {
		if unserved {
			_ = os.Remove(profiles.ParkedMarkerPath(h))
			name := filepath.Base(h)
			ack := gateway.RequestServeProfileHot(controlHome, name)
			if !gatewaycli.AckConfirmed(ack, "served", name) {
				cliLogger.Warn(fmt.Sprintf("Profile re-serve after snapshot restore: %s", pyrepr.Repr(ack)))
				printLines(fmt.Sprintf("  Profile '%s' is not served again yet: %s. The host retries on its next rescan (within 30s).",
					name, gatewaycli.AckFailure(ack)))
			}
		}
		store.ForgetSessions()
		if rerr := c.rejoinAfterSnapshot(store); rerr != nil {
			cliLogger.Warn(fmt.Sprintf("Rejoin after snapshot restore failed: %v", rerr))
			printLines(fmt.Sprintf("  Reconnecting to the gateway failed: %s. Run /new or restart the CLI.", rerr.Error()))
		}
	}()
	ours, err := pauseSnapshotOwner(h, controlHome, identity)
	if err != nil {
		return false, err
	}
	unserved = ours
	unowned, err := manage.WaitProfileUnowned(h, nil)
	if err != nil {
		return false, err
	}
	if !unowned {
		return false, &gateway.GatewayClientError{Msg: "Gateway still owns the profile; nothing was restored. Wait for its running work to stop and retry."}
	}
	return backup.RestoreQuickSnapshot(c.ctx, snapID, h)
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
