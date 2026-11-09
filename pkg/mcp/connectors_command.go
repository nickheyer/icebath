package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const connectorsPollSeconds = 2.0

var connectorsDoneStates = map[string]bool{"connected": true, "skipped": true, "failed": true, "expired": true, "not_connected": true}

const connectorsSignIn = "Run `icebath auth upgrade` to sign in."

var connectorsAliasRE = regexp.MustCompile(tools.AliasPattern)

// A user facing failure whose message is printed before the command exits 1
type connectorsCommandError struct{ msg string }

func (e *connectorsCommandError) Error() string { return e.msg }

func connectorsFail(message string) error { return &connectorsCommandError{msg: message} }

// Portal account reads and writes the command drives
type connectorsPortal interface {
	ListAccounts() ([]map[string]any, error)
	RenameAccount(connectionID, alias string) (map[string]any, error)
	DeleteAccount(connectionID string) (map[string]any, error)
}

// Portal client of the env, the constructor hermes tests patch
var connectorsPortalClient = func(ctx context.Context, env *tools.Env) connectorsPortal {
	return tools.NewPortalConnectorClient(ctx, env, tools.PortalOptions{})
}

// Account operation starter, the account module attribute hermes tests patch
var connectorsFindOrStart = tools.FindOrStartAccountOperation

// Connector availability probe, the module attribute hermes tests patch
var connectorsAvailable = tools.ConnectorsAvailable

// Stdin terminal probe the disconnect confirmation reads
var connectorsStdinIsTTY = func() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Confirmation line reader of the disconnect prompt
var connectorsInput = func(prompt string, w io.Writer) (string, error) {
	fmt.Fprint(w, prompt)
	var line string
	_, err := fmt.Fscanln(os.Stdin, &line)
	return line, err
}

// Validated account alias, a command error naming the shape otherwise
func connectorsValidAlias(alias string) (string, error) {
	if !connectorsAliasRE.MatchString(alias) {
		return "", connectorsFail(fmt.Sprintf("Invalid name %s. Use 1 to 32 lowercase letters, digits or hyphens, "+
			"starting with a letter or digit.", pyrepr.Repr(alias)))
	}
	return alias, nil
}

// Row value as text, empty for None
func connectorsRowText(row map[string]any, key string) string {
	v := row[key]
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

func connectorsAccountName(row map[string]any) string {
	for _, key := range []string{"alias", "label", "connectionId"} {
		if config.PyTruthy(row[key]) {
			return connectorsRowText(row, key)
		}
	}
	return ""
}

func connectorsDisplayName(row map[string]any) string {
	if config.PyTruthy(row["alias"]) {
		return connectorsRowText(row, "alias")
	}
	if config.PyTruthy(row["label"]) {
		return connectorsRowText(row, "label") + " (unnamed)"
	}
	return connectorsRowText(row, "connectionId") + " (unnamed)"
}

// Portal accounts of one app
func connectorsAppAccounts(ctx context.Context, env *tools.Env, app string) ([]map[string]any, error) {
	rows, err := connectorsPortalClient(ctx, env).ListAccounts()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, r := range rows {
		if pystr.Lower(connectorsRowText(r, "connector")) == app {
			out = append(out, r)
		}
	}
	return out, nil
}

func connectorsNamesHint(app string, rows []map[string]any) string {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[connectorsAccountName(r)] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 0 {
		return fmt.Sprintf("Its accounts are: %s.", strings.Join(names, ", "))
	}
	return fmt.Sprintf("You have no %s account.", app)
}

// The one account called name by alias, else the one unnamed account carrying name as its label
func connectorsMatchNamed(app string, rows []map[string]any, name string) (map[string]any, error) {
	matches := []map[string]any{}
	for _, r := range rows {
		if a, ok := r["alias"].(string); ok && a == name {
			matches = append(matches, r)
		}
	}
	if len(matches) == 0 {
		for _, r := range rows {
			if !config.PyTruthy(r["alias"]) && connectorsRowText(r, "label") == name {
				matches = append(matches, r)
			}
		}
	}
	if len(matches) != 1 {
		what := "No"
		if len(matches) > 0 {
			what = "More than one"
		}
		return nil, connectorsFail(fmt.Sprintf("%s %s account is called %s. %s", what, app, pyrepr.Repr(name), connectorsNamesHint(app, rows)))
	}
	return matches[0], nil
}

func connectorsList(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
	client, err := tools.NewManagedClient(env, true, "", nil)
	if err != nil {
		return 0, err
	}
	rows, err := client.ListConnectors(ctx, 0)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "No connectors are available.")
		return 0, nil
	}
	width := 0
	for _, r := range rows {
		width = max(width, len([]rune(connectorsRowText(r, "connector"))))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return connectorsRowText(rows[i], "connector") < connectorsRowText(rows[j], "connector")
	})
	for _, r := range rows {
		state := "not connected"
		if config.PyTruthy(r["connected"]) {
			state = "connected"
		}
		fmt.Fprintf(w, "%s  %s\n", pystr.Ljust(connectorsRowText(r, "connector"), width), state)
	}
	return 0, nil
}

func connectorsAccounts(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
	app := pystr.Lower(pystr.Strip(connectorsRowText(ns, "app")))
	all, err := connectorsPortalClient(ctx, env).ListAccounts()
	if err != nil {
		return 0, err
	}
	rows := []map[string]any{}
	for _, r := range all {
		if app == "" || pystr.Lower(connectorsRowText(r, "connector")) == app {
			rows = append(rows, r)
		}
	}
	retired, shown := []map[string]any{}, []map[string]any{}
	for _, r := range rows {
		if config.PyTruthy(r["disabled"]) {
			retired = append(retired, r)
		} else {
			shown = append(shown, r)
		}
	}
	showAll := config.PyTruthy(ns["all"])
	if showAll {
		shown = rows
	}
	if len(shown) == 0 && len(retired) == 0 {
		if app != "" {
			fmt.Fprintf(w, "You have no %s account.\n", app)
		} else {
			fmt.Fprintln(w, "You have no connected accounts.")
		}
		return 0, nil
	}
	sort.SliceStable(shown, func(i, j int) bool {
		ci, cj := connectorsRowText(shown[i], "connector"), connectorsRowText(shown[j], "connector")
		if ci != cj {
			return ci < cj
		}
		return connectorsAccountName(shown[i]) < connectorsAccountName(shown[j])
	})
	type tableRow struct{ connector, name, status string }
	table := make([]tableRow, 0, len(shown))
	for _, r := range shown {
		status := connectorsRowText(r, "status")
		if config.PyTruthy(r["disabled"]) {
			status += " (retired)"
		}
		table = append(table, tableRow{connectorsRowText(r, "connector"), connectorsDisplayName(r), status})
	}
	if len(table) > 0 {
		appWidth, nameWidth := 0, 0
		for _, row := range table {
			appWidth = max(appWidth, len([]rune(row.connector)))
			nameWidth = max(nameWidth, len([]rune(row.name)))
		}
		for _, row := range table {
			fmt.Fprintf(w, "%s  %s  %s\n", pystr.Ljust(row.connector, appWidth), pystr.Ljust(row.name, nameWidth), row.status)
		}
	}
	if len(retired) > 0 && !showAll {
		fmt.Fprintf(w, "%d retired (use --all to list)\n", len(retired))
	}
	return 0, nil
}

// First target row of the operation snapshot
func connectorsFirstTarget(snapshot *pyjson.Object) *pyjson.Object {
	targets, _ := snapshot.Get("targets")
	list, _ := targets.([]any)
	if len(list) == 0 {
		return pyjson.NewObject()
	}
	if t, ok := list[0].(*pyjson.Object); ok {
		return t
	}
	return pyjson.NewObject()
}

func connectorsTargetText(target *pyjson.Object, key string) string {
	v, _ := target.Get(key)
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Polls the operation until its one target settles, the browser link printed once, the last target returned
func connectorsWaitForLink(ctx context.Context, start *tools.AccountOperationStart, w io.Writer) (*pyjson.Object, error) {
	printedURL := false
	for {
		snapshot := start.Operation.Snapshot(true)
		target := connectorsFirstTarget(snapshot)
		if url := connectorsTargetText(target, "connect_url"); url != "" && !printedURL {
			fmt.Fprintln(w, url)
			fmt.Fprintln(w, "Open this link in your browser to finish.")
			printedURL = true
		}
		settledAt, _ := snapshot.Get("settled_at")
		if connectorsDoneStates[connectorsTargetText(target, "state")] || settledAt != nil {
			return target, nil
		}
		if float64(time.Now().UnixNano())/1e9 >= start.Operation.DeadlineAt {
			return target, nil
		}
		select {
		case <-ctx.Done():
			return target, ctx.Err()
		case <-time.After(time.Duration(connectorsPollSeconds * float64(time.Second))):
		}
	}
}

func connectorsReportConnect(app, alias string, target *pyjson.Object, w, errw io.Writer) int {
	title := tools.AppTitle(app)
	switch connectorsTargetText(target, "state") {
	case "connected":
		if alias != "" {
			fmt.Fprintf(w, "%s account %s is connected. Rename it anytime: icebath connectors rename %s %s --to <new-name>\n", title, alias, app, alias)
		} else {
			fmt.Fprintf(w, "%s is connected.\n", title)
		}
		return 0
	case "skipped":
		fmt.Fprintf(w, "Skipped connecting %s.\n", title)
		return 0
	}
	detail := connectorsTargetText(target, "detail")
	if detail == "" {
		detail = title + " did not finish connecting."
	}
	fmt.Fprintln(errw, detail)
	return 1
}

func connectorsConnect(ctx context.Context, env *tools.Env, ns map[string]any, w, errw io.Writer) (int, error) {
	app := pystr.Lower(pystr.Strip(connectorsRowText(ns, "app")))
	var start *tools.AccountOperationStart
	var err error
	var alias *string
	name := ""
	if config.PyTruthy(ns["reconnect"]) {
		row, err := connectorsPickAccount(ctx, env, app, connectorsOptText(ns, "alias"), false)
		if err != nil {
			return 0, err
		}
		name = connectorsAccountName(row)
		if connectorsRowText(row, "status") == "active" && !config.PyTruthy(ns["force"]) {
			fmt.Fprintf(w, "%s account %s is connected. Nothing to repair; pass --force to sign in again.\n", app, name)
			return 0, nil
		}
		repairID := connectorsRowText(row, "connectionId")
		start, err = connectorsFindOrStart(ctx, env, []string{app}, "reconnect", "", nil, &repairID, "")
		if err != nil {
			return 0, err
		}
	} else {
		if raw := connectorsOptText(ns, "alias"); raw != nil {
			valid, err := connectorsValidAlias(*raw)
			if err != nil {
				return 0, err
			}
			alias = &valid
			name = valid
		}
		start, err = connectorsFindOrStart(ctx, env, []string{app}, "connect", "", alias, nil, "")
		if err != nil {
			return 0, err
		}
	}
	tools.WaitForAccountPrepare(start)
	if start.Failed() {
		return 0, connectorsFail(fmt.Sprintf("Could not start connecting %s. Try again in a moment.", app))
	}
	target, err := connectorsWaitForLink(ctx, start, w)
	if err != nil {
		start.Operation.Settle(tools.SettleContinue)
		tools.LiveClose(start.Operation)
		fmt.Fprintln(w, "\nStopped waiting.")
		return 130, nil
	}
	return connectorsReportConnect(app, name, target, w, errw), nil
}

// Optional text of a namespace value, nil for None
func connectorsOptText(ns map[string]any, key string) *string {
	if ns[key] == nil {
		return nil
	}
	s := connectorsRowText(ns, key)
	return &s
}

func connectorsRename(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
	app := pystr.Lower(pystr.Strip(connectorsRowText(ns, "app")))
	newName, err := connectorsValidAlias(connectorsRowText(ns, "to"))
	if err != nil {
		return 0, err
	}
	rows, err := connectorsAppAccounts(ctx, env, app)
	if err != nil {
		return 0, err
	}
	row, err := connectorsMatchNamed(app, rows, connectorsRowText(ns, "name"))
	if err != nil {
		return 0, err
	}
	if _, err := connectorsPortalClient(ctx, env).RenameAccount(connectorsRowText(row, "connectionId"), newName); err != nil {
		var ge *tools.ToolGatewayError
		if errors.As(err, &ge) {
			switch {
			case ge.Kind == "idempotency":
				return 0, connectorsFail(fmt.Sprintf("The name %s is already used by another %s account.", newName, app))
			case ge.Kind == "unavailable" && ge.Code == "connection_not_found":
				return 0, connectorsFail("That account no longer exists.")
			case ge.Kind == "unavailable", ge.Status == 405:
				return 0, connectorsFail("This Nous Portal cannot rename connector accounts yet.")
			}
		}
		return 0, err
	}
	fmt.Fprintf(w, "Renamed %s account %s to %s.\n", app, connectorsRowText(ns, "name"), newName)
	return 0, nil
}

// The named account, else the one live account of the app, retired ones pickable by name only
func connectorsPickAccount(ctx context.Context, env *tools.Env, app string, alias *string, retired bool) (map[string]any, error) {
	rows, err := connectorsAppAccounts(ctx, env, app)
	if err != nil {
		return nil, err
	}
	if alias != nil {
		eligible := []map[string]any{}
		for _, r := range rows {
			if retired || !config.PyTruthy(r["disabled"]) {
				eligible = append(eligible, r)
			}
		}
		return connectorsMatchNamed(app, eligible, *alias)
	}
	live := []map[string]any{}
	for _, r := range rows {
		if !config.PyTruthy(r["disabled"]) {
			live = append(live, r)
		}
	}
	if len(live) == 1 {
		return live[0], nil
	}
	if len(live) == 0 {
		return nil, connectorsFail(fmt.Sprintf("You have no %s account.", app))
	}
	return nil, connectorsFail(fmt.Sprintf("You have more than one %s account. Pass --alias to pick one. %s", app, connectorsNamesHint(app, live)))
}

func connectorsDisconnect(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
	app := pystr.Lower(pystr.Strip(connectorsRowText(ns, "app")))
	row, err := connectorsPickAccount(ctx, env, app, connectorsOptText(ns, "alias"), true)
	if err != nil {
		return 0, err
	}
	name := connectorsAccountName(row)
	if !config.PyTruthy(ns["yes"]) {
		if !connectorsStdinIsTTY() {
			return 0, connectorsFail("Refusing to remove an account without confirmation. Pass --yes to confirm.")
		}
		answer, _ := connectorsInput(fmt.Sprintf("Remove %s account %s? This signs Icebath out of it. [y/N] ", app, name), w)
		if a := pystr.Lower(pystr.Strip(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(w, "Nothing removed.")
			return 0, nil
		}
	}
	if _, err := connectorsPortalClient(ctx, env).DeleteAccount(connectorsRowText(row, "connectionId")); err != nil {
		return 0, err
	}
	fmt.Fprintf(w, "Removed %s account %s.\n", app, name)
	return 0, nil
}

// Runs icebath connectors from its argparse namespace, user facing failures printed on stderr with exit 1
func ConnectorsCommand(ctx context.Context, env *tools.Env, ns map[string]any, w, errw io.Writer) (int, error) {
	action := connectorsRowText(ns, "connectors_action")
	if action == "" {
		action = "list"
	}
	var handler func(context.Context, *tools.Env, map[string]any, io.Writer) (int, error)
	switch action {
	case "list", "ls":
		handler = connectorsList
	case "accounts":
		handler = connectorsAccounts
	case "connect":
		handler = func(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
			return connectorsConnect(ctx, env, ns, w, errw)
		}
	case "rename":
		handler = connectorsRename
	case "disconnect":
		handler = connectorsDisconnect
	default:
		fmt.Fprintf(errw, "Unknown connectors action: %s\n", action)
		return 2, nil
	}
	if !connectorsAvailable(env) {
		fmt.Fprintf(errw, "Connectors are not available. %s\n", connectorsSignIn)
		return 1, nil
	}
	rc, err := handler(ctx, env, ns, w)
	if err == nil {
		return rc, nil
	}
	var cmdErr *connectorsCommandError
	var ge *tools.ToolGatewayError
	switch {
	case errors.As(err, &cmdErr):
		fmt.Fprintln(errw, cmdErr.msg)
	case errors.As(err, &ge) && ge.Kind == "auth":
		fmt.Fprintln(errw, connectorsSignIn)
	case errors.As(err, &ge):
		fmt.Fprintln(errw, ge.Error())
	default:
		return rc, err
	}
	return 1, nil
}
