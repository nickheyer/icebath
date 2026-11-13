package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ManagedWatchTickSeconds reads each pending account once a second, inside the account route budget
var ManagedWatchTickSeconds = 1.0

const (
	managedMinReadSeconds = 1.0
	managedMaxReadSeconds = 10.0
)

type managedOutcome struct {
	to    TargetState
	actor ConnActor
}

var managedAccountOutcome = map[string]managedOutcome{
	"active":   {TargetConnected, ActorBackendWatcher},
	"failed":   {TargetFailed, ActorBackendWatcher},
	"revoked":  {TargetFailed, ActorBackendWatcher},
	"inactive": {TargetFailed, ActorBackendWatcher},
	"expired":  {TargetExpired, ActorClock},
}

// ManagedNote is the model guidance appended to a settled managed connector result
const ManagedNote = "Settled once. connected → use the app now; skipped → the user chose Not now, do not connect it " +
	"or route around it; not_connected → ask the user what to do, never re-mint on your own. " +
	"A later request from the USER for that same app is not a re-ask — run it. " +
	"If you named an account, tell the user its name and that they can rename it."

// GuestSetupFailed is the error code of a guest identity that could not be created for a connect
const GuestSetupFailed = "GUEST_SETUP_FAILED"

// Connect request initiators, the agent tool, the desktop Connectors page and the card's Try again
const (
	InitiatorAgentTool   = "agent_tool"
	InitiatorDesktopPage = "desktop_page"
	InitiatorCardRetry   = "card_retry"
)

// NoteAppConnected counts a finished connect toward the guest connector offer, installed by pkg/mcp over pkg/provider
var NoteAppConnected func(ctx context.Context, env *Env)

// NoteConnectedAppsSeen counts a list read showing a guest a connected app, installed by pkg/mcp over pkg/provider
var NoteConnectedAppsSeen func(ctx context.Context, env *Env, rows []any)

// ManagedClient is the gateway surface a managed connector operation calls
type ManagedClient interface {
	ConnectorLister
	Connections(ctx context.Context, req ConnectionsRequest) (map[string]any, error)
	AccountStatus(ctx context.Context, connectionID string, timeout time.Duration) (map[string]any, error)
}

// PortalAccountClient lists and renames Portal connector accounts
type PortalAccountClient interface {
	ListAccounts() ([]map[string]any, error)
	RenameAccount(connectionID, alias string) (map[string]any, error)
}

// PortalAccountClientFactory builds the Portal account client managed actions read, the seam tests replace
var PortalAccountClientFactory = func(ctx context.Context, env *Env) PortalAccountClient {
	return NewPortalConnectorClient(ctx, env, PortalOptions{})
}

// PortalAccounts lists the caller's Portal connector accounts, the module attribute tests replace
var PortalAccounts = func(ctx context.Context, env *Env) ([]map[string]any, error) {
	return PortalAccountClientFactory(ctx, env).ListAccounts()
}

// Local catalog names and hosted slugs as the managed module reads them, the attributes tests replace
var (
	managedCatalogNames = CatalogNames
	managedHostedNames  = HostedNames
)

// connRaised carries an error out of an operation hook the way a Python exception leaves the loop
type connRaised struct{ err error }

func connRaise(err error) { panic(connRaised{err}) }

// Creates the guest identity before a managed client is built, empty on success
var EnsureGuestIdentity = ensureGuestIdentity

// Real gateway client for a user action, creating the guest identity first when createsGuest allows it
func NewManagedClient(env *Env, createsGuest bool, initiator string, call *CallRef) (*ConnectorClient, error) {
	if createsGuest {
		if msg := EnsureGuestIdentity(env); msg != "" {
			return nil, newToolGatewayError("", msg, GuestSetupFailed, 0, "", true)
		}
	}
	return NewConnectorClient(env, call, initiator), nil
}

type managedRun struct {
	ctx    context.Context
	env    *Env
	client ManagedClient
}

// Prepare and observe hooks for a connect or reconnect, each counting fresh connections once
func ManagedKind(ctx context.Context, env *Env, client ManagedClient, action string, force bool) ConnKind {
	m := &managedRun{ctx: ctx, env: env, client: client}
	noted := map[string]bool{}
	noting := func(step func(op *ConnectionOperation)) func(op *ConnectionOperation) {
		return func(op *ConnectionOperation) {
			step(op)
			m.noteConnected(op, noted)
		}
	}
	return ConnKind{Prepare: noting(m.prepare(action, force)), Observe: noting(m.observe), Note: ManagedNote}
}

func (m *managedRun) noteConnected(op *ConnectionOperation, noted map[string]bool) {
	fresh := false
	for _, t := range op.Targets {
		if op.StateOf(t) == TargetConnected && !noted[t.Name] {
			noted[t.Name] = true
			fresh = true
		}
	}
	if fresh {
		NoteAppConnected(m.ctx, m.env)
	}
}

func (m *managedRun) statusBySlug() map[string]map[string]any {
	items, err := m.client.ListConnectors(m.ctx, 0)
	if err != nil {
		connRaise(err)
	}
	out := map[string]map[string]any{}
	for _, item := range items {
		if item != nil {
			out[connectorSlug(item)] = item
		}
	}
	return out
}

// Mints links for names and applies the gateway's per app answer, a repair restarting one account
func Mint(ctx context.Context, env *Env, client ManagedClient, op *ConnectionOperation, names []string, reinitiate bool, actor ConnActor, connectionID string) error {
	if len(names) == 0 {
		return nil
	}
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	var namedAlias, namedConnection, repaired string
	hasNamed := false
	for _, t := range op.Targets {
		op.UpdateTarget(t, func(t *ConnTarget) {
			if !wanted[t.Name] {
				return
			}
			if !hasNamed && t.Alias != nil && *t.Alias != "" {
				hasNamed, namedAlias, namedConnection = true, *t.Alias, t.ConnectionID
			}
			if repaired == "" && t.RepairID != nil && *t.RepairID != "" {
				repaired = *t.RepairID
			}
		})
	}
	if reinitiate && connectionID == "" {
		connectionID = repaired
		if connectionID == "" && hasNamed {
			connectionID = namedConnection
		}
	}
	alias := ""
	if connectionID == "" && hasNamed {
		alias = namedAlias
	}
	returnTo, opArg := ReturnToArgs(env, op.OpID)
	response, err := client.Connections(ctx, ConnectionsRequest{Connectors: names, Reinitiate: reinitiate, Alias: alias,
		ConnectionID: connectionID, ReturnTo: returnTo, Op: opArg})
	if err != nil {
		return err
	}
	for _, entry := range connEntries(response["results"]) {
		name := pystr.Lower(connStrOr(entry["connector"]))
		target := op.Target(name)
		if target == nil {
			continue
		}
		status := connStrOr(entry["status"])
		detail := connStrOr(entry["status_reason"])
		if status != "active" && status != "initiated" && detail == "" {
			detail = connStrOr(entry["instruction"])
		}
		connID := connOptString(entry["connection_id"])
		var steps []func() error
		switch {
		case status == "active":
			steps = append(steps, connStep(op, name, TargetInitiated, actor, TransitionFields{}),
				connStep(op, name, TargetConnected, ActorBackendWatcher, TransitionFields{ConnectionID: connID}))
		case status == "initiated":
			if connID == nil || *connID == "" {
				envLog(env).Warn(fmt.Sprintf("connector %s: the mint named no account, so the watcher cannot read it; "+
					"only the card or the deadline can end the row", name))
			}
			steps = append(steps, connStep(op, name, TargetInitiated, actor,
				TransitionFields{ConnectURL: connOptString(entry["connect_url"]), ConnectionID: connID, Detail: &detail}))
		case op.StateOf(target) == TargetFailed:
			steps = append(steps, func() error { return op.Refresh(name, "", detail, ActorUser, "") })
		case op.StateOf(target) == TargetExpired:
			steps = append(steps, connStep(op, name, TargetInitiated, actor, TransitionFields{}),
				connStep(op, name, TargetFailed, ActorBackendWatcher, TransitionFields{Detail: &detail}),
				func() error { return op.Refresh(name, "", detail, ActorUser, "") })
		default:
			steps = append(steps, connStep(op, name, TargetFailed, ActorBackendWatcher, TransitionFields{Detail: &detail}))
		}
		for _, step := range steps {
			if err := step(); err != nil {
				return err
			}
		}
	}
	return nil
}

func connStep(op *ConnectionOperation, name string, to TargetState, actor ConnActor, f TransitionFields) func() error {
	return func() error {
		_, err := op.Transition(name, to, actor, f)
		return err
	}
}

// Rows of a decoded results list
func connEntries(v any) []map[string]any {
	switch rows := v.(type) {
	case []map[string]any:
		return rows
	case []any:
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			if m, ok := r.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// Pointer to a string value, nil for Python None or a non string
func connOptString(v any) *string {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}

// One account read for a target, nil with nothing to apply, the 429 error returned to the tick
func (m *managedRun) statusFor(op *ConnectionOperation, t *ConnTarget, timeout float64) (map[string]any, *ToolGatewayError) {
	var connID string
	var next float64
	op.UpdateTarget(t, func(t *ConnTarget) { connID, next = t.ConnectionID, t.NextReadAt })
	if connID == "" || connNow() < next {
		return nil, nil
	}
	row, err := m.client.AccountStatus(m.ctx, connID, time.Duration(timeout*float64(time.Second)))
	if err != nil {
		if limited, ok := gatewayErrorKind(err, gatewayKindRateLimited); ok {
			return nil, limited
		}
		envLog(m.env).Warn(fmt.Sprintf("connector account read failed for %s: %s", t.Name, err))
		return nil, nil
	}
	return row, nil
}

// Applies one account read to one target, dropping a read the card already overtook
func managedApplyRead(env *Env, op *ConnectionOperation, t *ConnTarget, status, reason string) error {
	outcome, ok := managedAccountOutcome[status]
	if !ok {
		return nil
	}
	err := func() error {
		if AllowedTransition(t.Kind, op.StateOf(t), outcome.to) == "" {
			if _, err := op.Transition(t.Name, TargetInitiated, ActorBackendWatcher, TransitionFields{}); err != nil {
				return err
			}
		}
		detail := reason
		if detail == "" {
			op.UpdateTarget(t, func(t *ConnTarget) { detail = t.Detail })
		}
		_, err := op.Transition(t.Name, outcome.to, outcome.actor, TransitionFields{Detail: &detail})
		return err
	}()
	var illegal *IllegalTransition
	if err != nil && errors.As(err, &illegal) {
		if !op.Settled() && managedLive(op, t) {
			return err
		}
		envLog(env).Debug(fmt.Sprintf("connector %s: %s read dropped, the row is %s", t.Name, status, op.StateOf(t)))
		return nil
	}
	return err
}

func managedLive(op *ConnectionOperation, t *ConnTarget) bool {
	state := op.StateOf(t)
	return state == TargetPending || state == TargetInitiated
}

func managedPark(op *ConnectionOperation, until float64) {
	for _, t := range op.Targets {
		if managedLive(op, t) {
			op.UpdateTarget(t, func(t *ConnTarget) { t.NextReadAt = until })
		}
	}
}

func (m *managedRun) observe(op *ConnectionOperation) {
	for _, t := range op.Targets {
		if op.Settled() || !managedLive(op, t) {
			continue
		}
		timeout := min(managedMaxReadSeconds, max(managedMinReadSeconds, op.RemainingSeconds()))
		row, limited := m.statusFor(op, t, timeout)
		if limited != nil {
			managedPark(op, connNow()+limited.RetryAfter)
			return
		}
		if row == nil {
			continue
		}
		if err := managedApplyRead(m.env, op, t, pystr.Lower(connStrOr(row["status"])), connStrOr(row["statusReason"])); err != nil {
			connRaise(err)
		}
	}
}

func managedUnminted(op *ConnectionOperation) []*ConnTarget {
	var out []*ConnTarget
	for _, t := range op.Targets {
		var connID string
		op.UpdateTarget(t, func(t *ConnTarget) { connID = t.ConnectionID })
		if op.StateOf(t) == TargetFailed && connID == "" {
			out = append(out, t)
		}
	}
	return out
}

func (m *managedRun) markMisrouted(op *ConnectionOperation) {
	unminted := managedUnminted(op)
	if len(unminted) == 0 {
		return
	}
	catalog := managedCatalogNames()
	var candidates []*ConnTarget
	for _, t := range unminted {
		if catalog[t.Name] {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return
	}
	hosted, ok := managedHostedNames(m.ctx, m.env)
	if !ok {
		return
	}
	misrouted := 0
	for _, t := range candidates {
		if hosted[t.Name] {
			continue
		}
		misrouted++
		if err := op.Refresh(t.Name, "", MisroutedToHostedError(t.Name), ActorBackendWatcher, ""); err != nil {
			connRaise(err)
		}
	}
	if misrouted > 0 && misrouted == len(op.Targets) {
		op.Settle(SettleAllResolved)
	}
}

func managedUnknownSlug(name string, known map[string]bool) string {
	var near []string
	for slug := range known {
		if strings.Contains(slug, name) || strings.Contains(name, slug) {
			near = append(near, slug)
		}
	}
	sort.Strings(near)
	if len(near) > 0 {
		return fmt.Sprintf("'%s' is not a connector slug; use %s", name, strings.Join(near, " or "))
	}
	return fmt.Sprintf("'%s' is not a connector slug; manage_connections status lists them", name)
}

func (m *managedRun) markUnknown(op *ConnectionOperation) {
	unminted := managedUnminted(op)
	if op.Settled() || len(unminted) == 0 {
		return
	}
	catalog := managedCatalogNames()
	var local []*ConnTarget
	for _, t := range unminted {
		if !catalog[t.Name] {
			local = append(local, t)
		}
	}
	if len(local) == 0 {
		return
	}
	items, err := m.client.ListConnectors(m.ctx, 0)
	if err != nil {
		var ge *ToolGatewayError
		if errors.As(err, &ge) {
			envLog(m.env).Debug(fmt.Sprintf("connector list for an unminted slug failed: %s", err))
			return
		}
		connRaise(err)
	}
	known := map[string]bool{}
	for _, item := range items {
		known[connectorSlug(item)] = true
	}
	unknown := 0
	for _, t := range local {
		if known[t.Name] {
			continue
		}
		unknown++
		if err := op.Refresh(t.Name, "", managedUnknownSlug(t.Name, known), ActorBackendWatcher, ""); err != nil {
			connRaise(err)
		}
	}
	if unknown > 0 && unknown == len(op.Targets) {
		op.Settle(SettleAllResolved)
	}
}

func (m *managedRun) mint(op *ConnectionOperation, names []string, reinitiate bool, connectionID string) {
	if err := Mint(m.ctx, m.env, m.client, op, names, reinitiate, ActorBackendWatcher, connectionID); err != nil {
		connRaise(err)
	}
	m.markMisrouted(op)
	m.markUnknown(op)
}

func (m *managedRun) prepare(action string, force bool) func(op *ConnectionOperation) {
	return func(op *ConnectionOperation) {
		names := make([]string, len(op.Targets))
		for i, t := range op.Targets {
			names[i] = t.Name
		}
		if action == "connect" {
			m.mint(op, names, false, "")
			return
		}
		named, order := m.aliasStatus(op)
		if force {
			account := ""
			for _, name := range order {
				if id, ok := named[name]["connection_id"].(string); ok && id != "" {
					account = id
					break
				}
			}
			m.mint(op, names, true, account)
			return
		}
		status := named
		if len(named) == 0 {
			status = m.statusBySlug()
		}
		var repair []string
		for _, name := range names {
			target := op.Target(name)
			repairID := ""
			if target != nil {
				op.UpdateTarget(target, func(t *ConnTarget) {
					if t.RepairID != nil {
						repairID = *t.RepairID
					}
				})
			}
			_, isNamed := named[name]
			switch {
			case target != nil && repairID != "" && !isNamed:
				repair = append(repair, name)
			case truthy(status[name]["connected"]):
				for _, to := range []TargetState{TargetInitiated, TargetConnected} {
					if _, err := op.Transition(name, to, ActorBackendWatcher, TransitionFields{}); err != nil {
						connRaise(err)
					}
				}
			default:
				repair = append(repair, name)
			}
		}
		account := ""
		for _, name := range repair {
			if id := status[name]["connection_id"]; connTruthy(id) {
				account = pyrepr.Str(id)
				break
			}
		}
		m.mint(op, repair, true, account)
	}
}

// Connected flag and account id per aliased target read from that account's own row, nil when none is aliased
func (m *managedRun) aliasStatus(op *ConnectionOperation) (map[string]map[string]any, []string) {
	var aliased []*ConnTarget
	for _, t := range op.Targets {
		if t.Alias != nil && *t.Alias != "" {
			aliased = append(aliased, t)
		}
	}
	if len(aliased) == 0 {
		return nil, nil
	}
	rows, err := PortalAccounts(m.ctx, m.env)
	if err != nil {
		connRaise(err)
	}
	status := map[string]map[string]any{}
	var order []string
	for _, t := range aliased {
		var mine []map[string]any
		for _, r := range rows {
			if r["connector"] == t.Name && !truthy(r["disabled"]) {
				mine = append(mine, r)
			}
		}
		var row map[string]any
		for _, r := range mine {
			if r["alias"] == *t.Alias {
				row = r
				break
			}
		}
		if row == nil {
			for _, r := range mine {
				if !connTruthy(r["alias"]) && r["label"] == *t.Alias {
					row = r
					break
				}
			}
			if row != nil {
				op.UpdateTarget(t, func(t *ConnTarget) { t.Alias, t.RepairID = nil, connOptString(row["connectionId"]) })
			}
		}
		entry := map[string]any{"connected": row != nil && row["status"] == "active", "connection_id": nil}
		if row != nil {
			entry["connection_id"] = row["connectionId"]
		}
		if _, seen := status[t.Name]; !seen {
			order = append(order, t.Name)
		}
		status[t.Name] = entry
	}
	return status, order
}

func managedTargets(targets []HostedTarget, action string) []*ConnTarget {
	out := make([]*ConnTarget, len(targets))
	for i, t := range targets {
		out[i] = NewConnTarget(t.Name, "connector", action)
		out[i].Alias = t.Alias
	}
	return out
}

func (m *managedRun) noCardResult(action string, targets []HostedTarget, force bool, sessionKey string) string {
	op := NewDetachedOperation(managedTargets(targets, action), sessionKey)
	m.prepare(action, force)(op)
	payload := op.Result(m.ctx, m.env, true)
	status := "settled"
	for _, t := range op.Targets {
		if op.StateOf(t) == TargetInitiated {
			status = "initiated"
			break
		}
	}
	payload.Set("status", status)
	note := "Show each connect_url to the user; they open it in a browser to authorize. Ask them to tell you " +
		"when they are done, then check with action 'status'. Do not call connect again for the same app."
	for _, t := range targets {
		if t.Alias != nil && *t.Alias != "" {
			note += " You named this account: tell the user its name and that they can rename it."
			break
		}
	}
	payload.Set("note", note)
	return connDumps(payload)
}

func (m *managedRun) statusResult(connectors []string) (string, error) {
	items, err := m.client.ListConnectors(m.ctx, 0)
	if err != nil {
		return "", err
	}
	rows := make([]any, len(items))
	for i, item := range items {
		rows[i] = item
	}
	NoteConnectedAppsSeen(m.ctx, m.env, rows)
	known := map[string]bool{}
	for _, item := range items {
		known[connectorSlug(item)] = true
	}
	unknown := pyjson.NewObject()
	for _, name := range connectors {
		if !known[name] {
			unknown.Set(name, managedUnknownSlug(name, known))
		}
	}
	if len(connectors) > 0 {
		wanted := map[string]bool{}
		for _, n := range connectors {
			wanted[n] = true
		}
		var kept []map[string]any
		for _, item := range items {
			if wanted[connectorSlug(item)] {
				kept = append(kept, item)
			}
		}
		items = kept
	}
	accounts, err := PortalAccounts(m.ctx, m.env)
	if err != nil {
		var ge *ToolGatewayError
		if !errors.As(err, &ge) {
			return "", err
		}
		envLog(m.env).Debug(fmt.Sprintf("connector accounts for status failed: %s", err))
	} else {
		bySlug := map[string][]any{}
		for _, row := range accounts {
			entry := pyjson.ObjectOf("alias", row["alias"], "label", row["label"], "status", row["status"], "active", row["active"])
			if truthy(row["disabled"]) {
				entry.Set("retired", true)
			}
			slug := connectorSlug(row)
			bySlug[slug] = append(bySlug[slug], entry)
		}
		for i, item := range items {
			merged := make(map[string]any, len(item)+1)
			for k, v := range item {
				merged[k] = v
			}
			merged["accounts"] = append([]any{}, bySlug[connectorSlug(item)]...)
			items[i] = merged
		}
	}
	listed := make([]any, len(items))
	for i, item := range items {
		listed[i] = item
	}
	out := pyjson.ObjectOf("connectors", listed, "hint",
		"connected=false means calls to that connector will return CONNECTION_REQUIRED. "+
			"Use action 'connect' to start an authorization. 'accounts' lists each account by its name "+
			"(alias) and the vendor's label; address an unnamed account by its label. A retired account was "+
			"replaced by a reconnect and is kept only for the record.")
	if unknown.Len() > 0 {
		out.Set("unknown", unknown)
	}
	return connDumps(out), nil
}

func (m *managedRun) displayResult(suggested []string, sessionID, toolCallID string, cb ConnCallback, call *CallRef) (string, error) {
	if cb == nil {
		raw, err := m.statusResult(nil)
		if err != nil {
			return "", err
		}
		parsed, err := pyjson.Loads(raw)
		if err != nil {
			return "", err
		}
		obj := parsed.(*pyjson.Object)
		obj.Set("note", OffDesktopNote)
		return connDumps(obj), nil
	}
	return RunDisplay(m.ctx, m.env, m.client, suggested, OperationSessionKey(m.env, sessionID), toolCallID, cb, call), nil
}

// Python equality of a decoded value with an optional string, None equal to a missing or null value
func connOptEqual(v any, s *string) bool {
	if s == nil {
		return v == nil
	}
	return v == *s
}

func connReprOpt(s *string) string {
	if s == nil {
		return "None"
	}
	return pyrepr.StrRepr(*s)
}

// Renames the one account of the target called alias, or labelled so when unnamed, with no card
func managedRenameResult(ctx context.Context, env *Env, target HostedTarget) (string, error) {
	all, err := PortalAccounts(ctx, env)
	if err != nil {
		return "", err
	}
	var rows, matches []map[string]any
	for _, r := range all {
		if connectorSlug(r) == target.Name {
			rows = append(rows, r)
		}
	}
	for _, r := range rows {
		if connOptEqual(r["alias"], target.Alias) {
			matches = append(matches, r)
		}
	}
	if len(matches) == 0 {
		for _, r := range rows {
			if !connTruthy(r["alias"]) && connOptEqual(r["label"], target.Alias) {
				matches = append(matches, r)
			}
		}
	}
	if len(matches) != 1 {
		seen := map[string]bool{}
		var names []string
		for _, r := range rows {
			v := r["alias"]
			if !connTruthy(v) {
				v = r["label"]
			}
			name := "None"
			if v != nil {
				name = pyrepr.Str(v)
			}
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		sort.Strings(names)
		found := fmt.Sprintf("The user has no %s account.", target.Name)
		if len(names) > 0 {
			found = fmt.Sprintf("Its accounts are: %s.", strings.Join(names, ", "))
		}
		what := "More than one"
		if len(matches) == 0 {
			what = "No"
		}
		return toolErrorJSON(fmt.Sprintf("%s %s account is called %s. %s", what, target.Name, connReprOpt(target.Alias), found), nil), nil
	}
	to := ""
	if target.To != nil {
		to = *target.To
	}
	account, err := PortalAccountClientFactory(ctx, env).RenameAccount(pyrepr.Str(matches[0]["connectionId"]), to)
	if err != nil {
		var ge *ToolGatewayError
		if !errors.As(err, &ge) {
			return "", err
		}
		switch {
		case ge.Kind == gatewayKindIdempotency:
			return toolErrorJSON(fmt.Sprintf("The name %s is already used by another %s account. Pick another name.", connReprOpt(target.To), target.Name), nil), nil
		case ge.Kind == gatewayKindUnavailable && ge.Code == "connection_not_found":
			return toolErrorJSON(fmt.Sprintf("That %s account no longer exists. Use action status to list accounts.", target.Name), nil), nil
		case ge.Kind == gatewayKindUnavailable || ge.Status == 405:
			return toolErrorJSON("This Nous Portal cannot rename connector accounts yet.", nil), nil
		}
		return "", err
	}
	return connDumps(pyjson.ObjectOf(
		"renamed", pyjson.ObjectOf("connector", account["connector"], "alias", account["alias"], "label", account["label"]),
		"notice", fmt.Sprintf("Tell the user their %s account %s is now called %s.", target.Name, connReprOpt(target.Alias), connReprOpt(target.To)),
	)), nil
}

// ManagedOptions are run_managed_action's keyword arguments, empty strings standing for None
type ManagedOptions struct {
	ClientFactory       func() (ManagedClient, error)
	SessionID           string
	ToolCallID          string
	TurnID              string
	Callback            ConnCallback
	ConnectorsAvailable func() bool
	CardSurface         bool
}

// Runs a hosted connector action, drawing a card when a callback exists and links in the result otherwise
func RunManagedAction(ctx context.Context, env *Env, action string, targets []HostedTarget, args map[string]any, opts ManagedOptions) (out string) {
	if opts.ConnectorsAvailable != nil && !opts.ConnectorsAvailable() {
		return toolErrorJSON("Connectors are not available in this session.", nil)
	}
	defer func() {
		if r := recover(); r != nil {
			raised, ok := r.(connRaised)
			if !ok {
				panic(r)
			}
			out = managedFailure(env, action, raised.err)
		}
	}()
	result, err := runManagedAction(ctx, env, action, targets, args, opts)
	if err != nil {
		return managedFailure(env, action, err)
	}
	return result
}

func managedFailure(env *Env, action string, err error) string {
	envLog(env).Warn(fmt.Sprintf("manage_connections %s failed: %s", action, err))
	var ge *ToolGatewayError
	if errors.As(err, &ge) && ge.Code == GuestSetupFailed {
		return toolErrorJSON(err.Error(), nil)
	}
	return toolErrorJSON(fmt.Sprintf("The connector gateway request failed: %s. "+
		"If this persists, the user can manage connections in the Nous Portal.", err), nil)
}

func runManagedAction(ctx context.Context, env *Env, action string, targets []HostedTarget, args map[string]any, opts ManagedOptions) (string, error) {
	if action == "rename" {
		if len(targets) == 0 {
			return "", errors.New("list index out of range")
		}
		return managedRenameResult(ctx, env, targets[0])
	}
	call := &CallRef{TurnID: opts.TurnID, ToolCallID: opts.ToolCallID}
	var client ManagedClient
	if opts.ClientFactory != nil {
		c, err := opts.ClientFactory()
		if err != nil {
			return "", err
		}
		client = c
	} else {
		c, err := NewManagedClient(env, true, InitiatorAgentTool, call)
		if err != nil {
			return "", err
		}
		client = c
	}
	m := &managedRun{ctx: ctx, env: env, client: client}
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	switch action {
	case "display":
		var cb ConnCallback
		if opts.CardSurface {
			cb = opts.Callback
		}
		return m.displayResult(names, opts.SessionID, opts.ToolCallID, cb, call)
	case "status":
		return m.statusResult(names)
	}
	if len(targets) == 0 {
		return toolErrorJSON(fmt.Sprintf("'%s' requires 'connectors': the connector slugs to authorize (e.g. [\"gmail\"]). "+
			"Use action 'status' to list them.", action), nil), nil
	}
	force := connTruthy(args["force"])
	sessionKey := OperationSessionKey(env, opts.SessionID)
	if opts.Callback == nil {
		return m.noCardResult(action, targets, force, sessionKey), nil
	}
	tick := ManagedWatchTickSeconds
	return RunOperation(ctx, env, managedTargets(targets, action), ManagedKind(ctx, env, client, action, force),
		RunOptions{SessionKey: sessionKey, ToolCallID: opts.ToolCallID, Callback: opts.Callback, TickSeconds: &tick, Call: call}), nil
}
