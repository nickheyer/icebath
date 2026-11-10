package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

var (
	fleetObligationLog     = slog.Default().With("logger", "hermes_cli.update_cmd")
	fleetCollectVersions   = func() []*pyjson.Object { return CollectFleetVersions(nil, nil) }
	fleetReadCheckoutSHA   = updaterecord.ReadCheckoutSHA
	fleetLatestReceipt     = updaterecord.ReadLatestReceipt
	fleetActiveProfileName = profiles.GetActiveProfileName
	fleetCheckoutContains  = CheckoutContains
	fleetHostOwesNoRestart = hostOwesNoGatewayRestart
)

// One gateway identity the restart obligation names, the profile empty text for an unmapped stopped gateway
type gatewayIdentity struct {
	kind    string
	profile any
}

var unmappedGateway = gatewayIdentity{"gateway", ""}

// Identity of a gateway profile value, an error for a value Python cannot hash into a set
func gatewayOf(profile any) (gatewayIdentity, error) {
	switch profile.(type) {
	case map[string]any, []any, *pyjson.Object:
		return gatewayIdentity{}, fmt.Errorf("TypeError: unhashable type: '%s'", pyrepr.TypeName(profile))
	}
	return gatewayIdentity{"gateway", profile}, nil
}

// Legacy per home breadcrumb of the restart obligation, read and cleared but never written
func fleetRestartPendingMarkerPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), updaterecord.ProfileMarkerName)
}

// Arms the host restart obligation, true when armed or nothing is owed, a nil runtimes meaning no inventory was taken
func writeFleetRestartPendingMarker(ctx context.Context, expectedSHA string, runtimes []any, owner string) (bool, error) {
	if runtimes != nil && len(runtimes) == 0 {
		return true, nil
	}
	marker := fleetRestartPendingMarkerPath(ctx)
	if testOwnsLiveCheckout(filepath.Dir(marker)) {
		fleetObligationLog.Debug("Skipping fleet-restart-pending obligation under pytest (live checkout)")
		return true, nil
	}
	return updaterecord.ArmHostObligation(ctx, marker, expectedSHA, runtimes, currentProfileName(ctx), owner)
}

// Profile whose CLI armed the obligation, default when unnamed and empty when the lookup fails
func currentProfileName(ctx context.Context) (name string) {
	defer func() {
		if r := recover(); r != nil {
			name = ""
		}
	}()
	if active := fleetActiveProfileName(ctx); active != "" {
		return active
	}
	return "default"
}

// Removes an update recovery breadcrumb, a missing file fine and any other failure logged
func clearMarkerFile(path, label string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fleetObligationLog.Debug(fmt.Sprintf("Could not clear %s marker: %s", label, pypath.OSErrorText(err)))
	}
}

// Discharges the obligation for the whole host, the legacy per home marker included
func clearFleetRestartPendingMarker(ctx context.Context) {
	updaterecord.ClearHostObligation()
	clearMarkerFile(fleetRestartPendingMarkerPath(ctx), "fleet-restart-pending")
}

// True when this host owes a fleet restart, armed from any profile's CLI
func fleetRestartObligationArmed(ctx context.Context) bool {
	if updaterecord.HostObligationPresent() {
		return true
	}
	info, err := os.Stat(fleetRestartPendingMarkerPath(ctx))
	return err == nil && info.Mode().IsRegular()
}

// Armed obligation as key value fields, the host record first then the legacy marker, not ok when unarmed or malformed
func fleetObligationFields(ctx context.Context) (map[string]string, bool) {
	if fields := updaterecord.ObligationFields(); fields != nil {
		return fields, true
	}
	if updaterecord.HostObligationPresent() {
		return nil, false
	}
	raw, err := os.ReadFile(fleetRestartPendingMarkerPath(ctx))
	if err != nil {
		return nil, false
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		return nil, false
	}
	legacy := map[string]string{}
	for _, line := range pystr.Splitlines(string(raw), false) {
		key, value, found := strings.Cut(line, "=")
		if _, seen := legacy[key]; !found || seen {
			return nil, false
		}
		legacy[key] = value
	}
	return legacy, true
}

// Head of the checkout from the version record, from git when that fails, empty when neither resolves
func currentCheckoutSHA(ctx context.Context) string {
	if sha, err := fleetReadCheckoutSHA(); err == nil {
		if sha == nil {
			return ""
		}
		return *sha
	}
	return captureHeadSHA(ctx, []string{"git"}, fleetProjectRoot())
}

// Current HEAD of cwd from git, empty when it cannot be resolved
func captureHeadSHA(ctx context.Context, gitCmd []string, cwd string) string {
	result, err := RunGit(ctx, gitCmd, []string{"rev-parse", "HEAD"}, ChildOptions{Dir: cwd, Capture: true, Text: true, Check: true})
	if err != nil {
		return ""
	}
	return pystr.Strip(result.Stdout)
}

// True when the receipt is from an update that did not finish cleanly
func receiptLooksUnfinished(receipt any) bool {
	exitCode, outcome := rowValue(receipt, "exit_code"), rowValue(receipt, "outcome")
	if !(exitCode == nil || pyrepr.Equal(exitCode, int64(0))) || outcome == "failed" || outcome == "partial" || outcome == "running" {
		return true
	}
	if restart := rowValue(receipt, "gateway_restart"); isRowDict(restart) && pyrepr.Truthy(rowValue(restart, "incomplete")) {
		return true
	}
	succeeded := (exitCode != nil && pyrepr.Equal(exitCode, int64(0))) || outcome == "success"
	return pyrepr.Truthy(rowValue(receipt, "stop_reason")) && !succeeded
}

// True when the receipt records a runtime at another sha than expected, the fleet matrix before an unfinished plan
func receiptReportsStaleRuntime(ctx context.Context, receipt any, expectedSHA string) bool {
	if !isRowDict(receipt) {
		return false
	}
	if expectedSHA == "" {
		expectedSHA = currentCheckoutSHA(ctx)
	}
	if expectedSHA == "" {
		return false
	}
	mismatch := func(codeSHA any) bool { return pyrepr.Truthy(codeSHA) && pyrepr.Str(codeSHA) != expectedSHA }
	if fleet, isList := rowValue(receipt, "fleet").([]any); isList && len(fleet) > 0 {
		for _, entry := range fleet {
			if isRowDict(entry) && !updaterecord.RowIsExternal(entry) && (rowValue(entry, "state") == "stale" || mismatch(rowValue(entry, "code_sha"))) {
				return true
			}
		}
		return false
	}
	if !receiptLooksUnfinished(receipt) {
		return false
	}
	plan := rowValue(receipt, "plan")
	if !isRowDict(plan) {
		return false
	}
	runtimes, _ := rowValue(plan, "runtimes").([]any)
	for _, runtime := range runtimes {
		if isRowDict(runtime) && mismatch(rowValue(runtime, "code_sha")) {
			return true
		}
	}
	return false
}

// Gateway identities the receipt owes after manual retention, not ok when any row leaves coverage unverified
func receiptOwedGateways(receipt any, pendingManual []any) (map[gatewayIdentity]bool, bool, error) {
	type entry struct {
		row         any
		defaultKind any
	}
	var plan any = map[string]any{}
	if p := rowValue(receipt, "plan"); pyrepr.Truthy(p) {
		if !isRowDict(p) {
			return nil, false, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(p))
		}
		plan = p
	}
	var entries []entry
	for i, source := range []any{rowValue(plan, "runtimes"), rowValue(receipt, "pending_manual_serves"), rowValue(receipt, "carried_manual_serves"), rowValue(receipt, "fleet")} {
		rows, err := rowsOf(source)
		if err != nil {
			return nil, false, err
		}
		for _, row := range rows {
			if i == 3 {
				entries = append(entries, entry{row, "gateway"})
			} else {
				entries = append(entries, entry{row, nil})
			}
		}
	}
	owed := map[gatewayIdentity]bool{}
	unverified := false
	for _, e := range entries {
		if !isRowDict(e.row) {
			unverified = true
			continue
		}
		kind := rowValueOr(e.row, "kind", e.defaultKind)
		profile := rowValue(e.row, "profile")
		supervisor, _ := rowValue(e.row, "supervisor").(string)
		pending := false
		for _, row := range pendingManual {
			pending = pending || pyrepr.Equal(row, e.row)
		}
		if (kind == "serve" || kind == "dashboard") && supervisedServeBackends[supervisor] && !pending {
			continue
		}
		if kind != "gateway" || !pyrepr.Truthy(profile) || profile == "unknown" {
			unverified = true
			continue
		}
		identity, err := gatewayOf(profile)
		if err != nil {
			return nil, false, err
		}
		owed[identity] = true
	}
	if unverified {
		return nil, false, nil
	}
	return owed, true, nil
}

// Gateway identities the live rows vouch for, not ok when any row is unidentified, a panic for an unhashable profile
func fleetCoveredGateways(fleet []any) (map[gatewayIdentity]bool, bool) {
	covered := map[gatewayIdentity]bool{}
	for _, row := range fleet {
		profile := rowValue(row, "profile")
		if !isRowDict(row) || !pyrepr.Truthy(profile) || profile == "unknown" {
			return nil, false
		}
		served, _ := rowValue(row, "served_profiles").([]any)
		for _, name := range append([]any{profile}, served...) {
			identity, err := gatewayOf(name)
			if err != nil {
				panic(err)
			}
			covered[identity] = true
		}
	}
	return covered, true
}

// Live fleet rows as generic values
func liveFleetRows() []any {
	rows := fleetCollectVersions()
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

// True when every owed gateway identity has a live successor at the expected sha in an accepted state
func liveFleetCoversReceipt(expectedSHA string, receipt any, owed map[gatewayIdentity]bool, owedKnown bool, acceptStates ...string) (covers bool) {
	if expectedSHA == "" {
		return false
	}
	if len(acceptStates) == 0 {
		acceptStates = []string{"current"}
	}
	defer func() {
		if r := recover(); r != nil {
			fleetObligationLog.Debug(fmt.Sprintf("Could not reconcile pending fleet identities: %v", r))
			covers = false
		}
	}()
	if !owedKnown {
		return false
	}
	if len(owed) == 0 {
		var plan any = map[string]any{}
		if p := rowValue(receipt, "plan"); pyrepr.Truthy(p) {
			plan = p
		}
		return pyrepr.Truthy(rowValue(plan, "runtimes"))
	}
	fleet := liveFleetRows()
	if len(fleet) == 0 {
		return false
	}
	for _, row := range fleet {
		if updaterecord.RowIsExternal(row) {
			continue
		}
		state, isStr := rowValue(row, "state").(string)
		accepted := false
		for _, want := range acceptStates {
			accepted = accepted || (isStr && state == want)
		}
		if !accepted || rowValue(row, "code_sha") != expectedSHA {
			return false
		}
	}
	covered, identified := fleetCoveredGateways(fleet)
	if !identified {
		return false
	}
	for identity := range owed {
		if !covered[identity] {
			return false
		}
	}
	return true
}

// Error of a malformed or unsupported restart inventory, which keeps the marker
type inventoryValueError struct{ msg string }

func (e *inventoryValueError) Error() string { return e.msg }

// Gateway identities a marker's inventory owes, not recorded when the marker carries no inventory
func markerOwedGateways(ctx context.Context, inventory any) (owed map[gatewayIdentity]bool, recorded bool, err error) {
	if inventory == nil {
		return nil, false, nil
	}
	if !isRowDict(inventory) || !pyrepr.Equal(rowValue(inventory, "version"), int64(1)) {
		return nil, false, &inventoryValueError{"unsupported fleet-restart inventory"}
	}
	runtimes, isList := rowValue(inventory, "runtimes").([]any)
	if !isList {
		return nil, false, &inventoryValueError{"fleet-restart inventory has no runtime list"}
	}
	owed = map[gatewayIdentity]bool{}
	for _, runtime := range runtimes {
		if !isRowDict(runtime) {
			return nil, false, &inventoryValueError{"fleet-restart inventory row is not an object"}
		}
		if runtimeOutsideGatewayEvidence(ctx, runtime) {
			continue
		}
		if rowValue(runtime, "kind") == "gateway" && pyrepr.Truthy(rowValue(runtime, "stopped_unmapped")) {
			owed[unmappedGateway] = true
			continue
		}
		profile, isStr := rowValue(runtime, "profile").(string)
		if rowValue(runtime, "kind") != "gateway" || !isStr || pystr.Strip(profile) == "" || profile == "unknown" {
			return nil, false, &inventoryValueError{"fleet-restart inventory row is not an identified gateway"}
		}
		owed[gatewayIdentity{"gateway", profile}] = true
	}
	return owed, true, nil
}

// Fleet coverage with an unhashable profile reported as an error
func coveredGateways(fleet []any) (covered map[gatewayIdentity]bool, identified bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			covered, identified, err = nil, false, fmt.Errorf("%v", r)
		}
	}()
	covered, identified = fleetCoveredGateways(fleet)
	return covered, identified, nil
}

// Settles an inventory less marker on a host with no live gateway while HEAD still holds the pulled code
func dischargeGatewaylessMarker(ctx context.Context, checkoutSHA, expectedSHA string) (discharged bool) {
	gatewayless := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				fleetObligationLog.Debug(fmt.Sprintf("Gateway-less host probe failed; keeping fleet-restart-pending marker: %v", r))
				gatewayless = false
			}
		}()
		contained := checkoutSHA == expectedSHA
		if !contained {
			var err error
			if contained, err = fleetCheckoutContains(ctx, expectedSHA); err != nil {
				panic(err)
			}
		}
		gatewayless = contained && fleetHostOwesNoRestart(ctx)
	}()
	if !gatewayless {
		return false
	}
	clearFleetRestartPendingMarker(ctx)
	fleetObligationLog.Debug(fmt.Sprintf("Fleet-restart-pending marker discharged: host runs no gateway at %s", pystr.Slice(checkoutSHA, 0, 10)))
	return true
}

// Settles the armed marker when its own inventory is provably served by the live fleet, clearing it when so
func markerOnlyRestartObsolete(ctx context.Context) (bool, error) {
	fields, armed := fleetObligationFields(ctx)
	if !armed {
		return false, nil
	}
	expectedSHA := pystr.Strip(fields["expected_sha"])
	inventoryText, present := fields["inventory"]
	if !present {
		inventoryText = "null"
	}
	doc, err := pyjson.Loads(inventoryText)
	if err != nil {
		return false, nil
	}
	owed, recorded, err := markerOwedGateways(ctx, pyjson.Plain(doc))
	if err != nil {
		return false, nil
	}
	if recorded && len(owed) == 0 {
		clearFleetRestartPendingMarker(ctx)
		fleetObligationLog.Debug("Fleet-restart-pending marker discharged: no gateway obligation recorded")
		return true, nil
	}
	if recorded && expectedSHA == "" {
		return false, nil
	}
	checkoutSHA := currentCheckoutSHA(ctx)
	if recorded && checkoutSHA != expectedSHA {
		contained, err := fleetCheckoutContains(ctx, expectedSHA)
		if err != nil {
			return false, err
		}
		if !contained {
			return false, nil
		}
	}
	targetSHA := checkoutSHA
	if targetSHA == "" {
		return false, nil
	}
	fleet, probeErr := probeLiveFleet()
	if probeErr != nil {
		fleetObligationLog.Debug(fmt.Sprintf("Fleet probe failed; keeping fleet-restart-pending marker: %v", probeErr))
		return false, nil
	}
	allExternal := true
	for _, row := range fleet {
		allExternal = allExternal && updaterecord.RowIsExternal(row)
	}
	if len(fleet) == 0 || (expectedSHA == "" && allExternal) {
		if recorded || expectedSHA == "" {
			return false, nil
		}
		return dischargeGatewaylessMarker(ctx, checkoutSHA, expectedSHA), nil
	}
	covered, identified, err := coveredGateways(fleet)
	if err != nil {
		return false, err
	}
	if !identified {
		return false, nil
	}
	var local []any
	for _, row := range fleet {
		if updaterecord.RowIsExternal(row) {
			continue
		}
		local = append(local, row)
		if rowValue(row, "state") != "current" || pyrepr.Str(rowValue(row, "code_sha")) != targetSHA {
			return false, nil
		}
	}
	if recorded {
		for identity := range owed {
			if identity != unmappedGateway && !covered[identity] {
				return false, nil
			}
		}
	}
	if recorded && len(owed) > 0 && owed[unmappedGateway] {
		localCovered, _, err := coveredGateways(local)
		if err != nil {
			return false, err
		}
		spare := false
		for identity := range localCovered {
			spare = spare || !owed[identity]
		}
		if !spare {
			return false, nil
		}
	}
	clearFleetRestartPendingMarker(ctx)
	fleetObligationLog.Debug(fmt.Sprintf("Fleet-restart-pending marker discharged: %d gateway(s) already serve %s", len(fleet), pystr.Slice(targetSHA, 0, 10)))
	return true, nil
}

// Live fleet rows, an error when the probe panics
func probeLiveFleet() (fleet []any, err error) {
	defer func() {
		if r := recover(); r != nil {
			fleet, err = nil, fmt.Errorf("%v", r)
		}
	}()
	return liveFleetRows(), nil
}

// Pulled sha when the restart phase of the receipt completed, empty when it did not or recorded none
func receiptRestartPhaseCompleted(receipt any) string {
	restart := rowValue(receipt, "gateway_restart")
	if !isRowDict(restart) || !pyrepr.Truthy(restart) {
		return ""
	}
	if pyrepr.Truthy(rowValue(restart, "incomplete")) || pyrepr.Truthy(rowValue(restart, "phase_error")) {
		return ""
	}
	var post any = map[string]any{}
	if p := rowValue(receipt, "post_update"); pyrepr.Truthy(p) {
		post = p
	}
	if sha := rowValue(post, "sha"); pyrepr.Truthy(sha) {
		return pyrepr.Str(sha)
	}
	return ""
}

// Receipt and its retained manual transfers, read from the stores when the caller holds neither
func receiptAndPending(ctx context.Context, receipt any, pendingManual []any, havePending bool) (any, []any, error) {
	if receipt == nil {
		receipt = map[string]any{}
		if latest := fleetLatestReceipt(ctx); len(latest) > 0 {
			receipt = latest
		}
	}
	if !havePending {
		var err error
		if pendingManual, err = RetainReceiptManualServes(ctx, receipt); err != nil {
			return nil, nil, err
		}
	}
	return receipt, pendingManual, nil
}

// True when update catch up still needs identity matched gateways at checkout HEAD
func pendingFleetRestartNeeded(ctx context.Context, receipt any, pendingManual []any, havePending bool) (bool, error) {
	receipt, pendingManual, err := receiptAndPending(ctx, receipt, pendingManual, havePending)
	if err != nil {
		return false, err
	}
	if fleetRestartObligationArmed(ctx) {
		obsolete, err := markerOnlyRestartObsolete(ctx)
		return !obsolete, err
	}
	owed, owedKnown, err := receiptOwedGateways(receipt, pendingManual)
	if err != nil {
		return false, err
	}
	if !receiptReportsStaleRuntime(ctx, receipt, "") {
		return false, nil
	}
	return !liveFleetCoversReceipt(currentCheckoutSHA(ctx), receipt, owed, owedKnown), nil
}

// True when an update still owes a fleet restart, a completed restart held to the code it pulled
func updateOwesFleetRestart(ctx context.Context, receipt any, pendingManual []any, havePending bool) (bool, error) {
	receipt, pendingManual, err := receiptAndPending(ctx, receipt, pendingManual, havePending)
	if err != nil {
		return false, err
	}
	if fleetRestartObligationArmed(ctx) {
		obsolete, err := markerOnlyRestartObsolete(ctx)
		return !obsolete, err
	}
	owed, owedKnown, err := receiptOwedGateways(receipt, pendingManual)
	if err != nil {
		return false, err
	}
	if !receiptReportsStaleRuntime(ctx, receipt, "") {
		return false, nil
	}
	if restartedTo := receiptRestartPhaseCompleted(receipt); restartedTo != "" && liveFleetCoversReceipt(restartedTo, receipt, owed, owedKnown, "current", "stale") {
		return false, nil
	}
	return !liveFleetCoversReceipt(currentCheckoutSHA(ctx), receipt, owed, owedKnown), nil
}

// Prints the interrupted update warning, with the remedy line at startup
func warnPendingFleetRestart(stream io.Writer, startup bool) {
	fmt.Fprintln(stream, "⚠ A previous `icebath update` pulled new code but did not restart running gateways.")
	fmt.Fprintln(stream, "  Gateways may still be serving pre-update modules (mixed sys.modules).")
	if startup {
		fmt.Fprintln(stream, "  Run `icebath update` or `icebath gateway restart`.")
	}
}

// Runs fn with any error or panic swallowed
func suppressed(fn func() error) {
	defer func() { _ = recover() }()
	_ = fn()
}

// Startup hint about a fleet restart or serve restart a previous update still owes, never restarting and never failing
func WarnPendingFleetRestartOnStartup(ctx context.Context, h string, w io.Writer) {
	if h != "" && h != home.Dir(ctx) {
		ctx = home.WithOverride(ctx, h)
	}
	var receipt any = map[string]any{}
	if latest := fleetLatestReceipt(ctx); len(latest) > 0 {
		receipt = latest
	}
	var pendingManual []any
	havePending := false
	suppressed(func() error {
		pending, err := RetainReceiptManualServes(ctx, receipt)
		if err == nil {
			pendingManual, havePending = pending, true
		}
		return err
	})
	suppressed(func() error {
		owes, err := updateOwesFleetRestart(ctx, receipt, pendingManual, havePending)
		if err == nil && owes {
			warnPendingFleetRestart(w, true)
		}
		return err
	})
	suppressed(func() error { return warnPendingManualServesTo(ctx, w, pendingManual, havePending) })
}
