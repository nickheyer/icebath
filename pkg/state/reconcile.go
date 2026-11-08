package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
)

// Gateway runtime ownership seams the host wires, the lock a browse reconcile and the explicit repair hold
var (
	ExclusiveMaintenance  func(ctx context.Context, homes []string, purpose string) (context.Context, func(), error)
	IsOwnershipConflict   func(err error) bool
	LiveRuntimeOwner      func(home string) (pid int, live bool, err error)
	CanonicalHome         func(home string) (string, error)
	ProfileLabelForHome   func(home string) string
	ReadGatewayLockRecord func(path string) *pyjson.Object
)

// Panics unless every gateway ownership seam a reconcile needs is wired
func requireOwnershipHooks() {
	requireHook(ExclusiveMaintenance != nil && IsOwnershipConflict != nil && LiveRuntimeOwner != nil &&
		CanonicalHome != nil && ProfileLabelForHome != nil && ReadGatewayLockRecord != nil,
		"ExclusiveMaintenance", "gateway runtime_ownership and status")
}

// Budget of a browse catch up, the holder purpose it publishes and the sibling poll interval
const (
	BrowseReconcileBudgetS = 3.0
	BrowseIntegrityBudgetS = BrowseReconcileBudgetS
	budgetProgressOps      = 1000
	BrowseReconcilePurpose = "browse_schema_reconcile"
	browseSiblingPollS     = 0.1
)

// RuntimeError the hermes reconcile raises for a refusal
func runtimeError(msg string) error { return &pyConvError{"RuntimeError", msg} }

// Python message of a RuntimeError, the bare text without the class prefix
func runtimeErrorText(err error) string {
	var conv *pyConvError
	if errors.As(err, &conv) {
		return conv.Msg
	}
	return errText(err)
}

// Seconds as a duration
func secondsOf(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Read only tracked connection to a store through its resolved file URI
func connectReadOnlyTracked(path string, timeout *float64) (*TrackedConn, error) {
	uri, err := ReadOnlyDBURI(path)
	if err != nil {
		return nil, err
	}
	opts := []TrackedOption{WithURI(), WithTrackingPath(path)}
	if timeout != nil {
		opts = append(opts, WithTimeout(*timeout))
	}
	return ConnectTracked(uri, opts...)
}

// Missing declared table or column of a store, drift true with its message, never a corruption diagnosis
func SchemaDriftReason(ctx context.Context, path string) (string, bool, error) {
	conn, err := connectReadOnlyTracked(path, nil)
	if err != nil {
		return "", false, err
	}
	defer conn.Close()
	statements, err := SchemaReadProbeStatements(ctx)
	if err != nil {
		return "", false, err
	}
	for _, statement := range statements {
		if _, err := fetchOne(ctx, conn, statement); err != nil {
			if isOperationalError(err) && schemaNotBuilt(err) {
				return errText(err), true, nil
			}
			return "", false, err
		}
	}
	return "", false, nil
}

// Snapshot copier the reconcile uses, a seam for tests that alter the staged copy
var reconcileCopySnapshot = copyDatabaseSnapshot

// Reconciles a scratch snapshot additively and promotes it only once complete and healthy
func reconcileSnapshot(ctx context.Context, path string, guard SQLiteConn) error {
	scratch := path + ".reconcile-scratch"
	if msg := repairScratchSpaceError(path); msg != "" {
		return runtimeError(msg)
	}
	if msg := unlinkDBTriple(scratch); msg != "" {
		return runtimeError(msg)
	}
	defer func() {
		if msg := unlinkDBTriple(scratch); msg != "" {
			slog.Warn("Could not remove reconciliation snapshot: " + msg)
		}
	}()
	if err := reconcileCopySnapshot(ctx, path, scratch, guard, nil); err != nil {
		return err
	}
	conn, err := connectRepairDurable(ctx, scratch, 5.0)
	if err != nil {
		return err
	}
	err = func() error {
		defer conn.Close()
		if err := requireHistoryTables(ctx, conn); err != nil {
			return err
		}
		if err := ApplyDeclaredSchema(ctx, conn); err != nil {
			return err
		}
		return endTransaction(ctx, conn, "COMMIT")
	}()
	if err != nil {
		return err
	}
	if reason, drift, err := SchemaDriftReason(ctx, scratch); err != nil {
		return err
	} else if drift {
		return runtimeError("schema reconciliation incomplete: " + reason)
	}
	if reason, sick, err := dbOpensCleanly(ctx, scratch); err != nil {
		return err
	} else if sick {
		return runtimeError("reconciled snapshot is unhealthy: " + reason)
	}
	return reconcileCopySnapshot(ctx, scratch, path, nil, guard)
}

// Refuses additive reconciliation of a store missing its canonical history tables
func requireHistoryTables(ctx context.Context, conn SQLiteConn) error {
	rows, err := queryObjects(ctx, conn, "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return err
	}
	tables := map[string]bool{}
	for _, row := range rows {
		tables[pyStr(rowValue(row, "name"))] = true
	}
	if !tables["sessions"] || !tables["messages"] {
		return runtimeError("canonical history tables are missing; use sessions recover, not additive reconciliation")
	}
	return nil
}

// Outcome of an explicit declarative repair, empty strings standing for the None values of the hermes report
type DeclarativeRepairReport struct {
	Repaired                    bool
	Strategy, BackupPath, Error string
}

// Explicit repair body under the repair lock, filling the report or failing with the refusal
func reconcileLocked(ctx context.Context, path string, backup bool, report *DeclarativeRepairReport) error {
	held, err := liveWriterHoldsDB(ctx, path)
	if err != nil {
		return err
	}
	if held {
		return runtimeError("a live writer still holds state.db; stop it and retry")
	}
	conn, err := connectReadOnlyTracked(path, nil)
	if err != nil {
		return err
	}
	err = func() error {
		defer conn.Close()
		return requireHistoryTables(ctx, conn)
	}()
	if err != nil {
		return err
	}
	if _, drift, err := SchemaDriftReason(ctx, path); err != nil {
		return err
	} else if !drift {
		report.Repaired, report.Strategy = true, "already_healthy"
		return nil
	}
	if reason, sick, err := dbOpensCleanly(ctx, path); err != nil {
		return err
	} else if sick {
		return runtimeError(reason)
	}
	if backup {
		backupPath, refusal := backupDBFile(path)
		if backupPath == "" {
			return runtimeError("pre-repair backup refused: " + refusal)
		}
		report.BackupPath = backupPath
	}
	return exclusiveRepairDBGuard(ctx, path, func(guard SQLiteConn, guardErr error) error {
		if guard == nil {
			return runtimeError("could not acquire exclusive state.db repair ownership: " + errText(guardErr))
		}
		if err := reconcileSnapshot(ctx, path, guard); err != nil {
			return err
		}
		report.Repaired, report.Strategy = true, "reconcile_declarative_schema"
		return nil
	})
}

// Store lagging the declared schema that browsing could not catch up, Owned when a running gateway holds it
type DeclarativeDriftError struct {
	Detail string
	Owned  bool
}

func (e *DeclarativeDriftError) Error() string { return e.Detail }

func (e *DeclarativeDriftError) PyTypeName() string { return "DeclarativeDriftError" }

// Whether an error is a no such table or column read probe failure, drift and never corruption
func IsDeclarativeDriftError(err error) bool { return isOperationalError(err) && schemaNotBuilt(err) }

// The sessions repair invocation that targets a home, additive only and without a time limit
func repairCommand(home string) string {
	label := ProfileLabelForHome(home)
	if label == "default" {
		return "icebath sessions repair"
	}
	if label != "" {
		return "icebath -p " + label + " sessions repair"
	}
	return "ICEBATH_HOME=" + home + " icebath sessions repair"
}

// Profile label of a home with the gateway restart command that targets it
func ownerRemedy(home string) (string, string) {
	label := ProfileLabelForHome(home)
	if label != "" && label != "default" {
		return label, "icebath -p " + label + " gateway restart"
	}
	if label == "" {
		label = home
	}
	return label, "icebath gateway restart"
}

// Restart command for the gateway holding a home's lock, naming the owner home a multiplexer recorded
func owningGatewayRestart(home string) (string, error) {
	canonical, err := CanonicalHome(home)
	if err != nil {
		return "", err
	}
	record := ReadGatewayLockRecord(filepath.Join(canonical, "gateway.lock"))
	if ownerHome, ok := rowValue(record, "hermes_home").(string); ok && ownerHome != "" {
		ownerCanonical, err := CanonicalHome(ownerHome)
		if err != nil {
			return "", err
		}
		if ownerCanonical != canonical {
			_, restart := ownerRemedy(ownerHome)
			return restart, nil
		}
	}
	_, restart := ownerRemedy(home)
	return restart, nil
}

// Description of a gateway process holding the store without this profile's lock, false when none
func locklessGatewayHolder(path string) (string, bool, error) {
	holders, err := foreignStateDBHolders(path)
	if err != nil {
		return "", false, err
	}
	for _, holder := range holders {
		if holder.PID <= 0 {
			continue
		}
		argv, ok := readProcArgv(holder.PID)
		if !ok {
			if cmd, err := proc.Cmdline(holder.PID); err == nil && len(cmd) > 0 {
				argv, ok = cmd, true
			}
		}
		if ok && looksLikeIcebath(argv) && slices.Contains(argv[1:], "gateway") {
			return DescribeHolderPID(holder.PID), true, nil
		}
	}
	return "", false, nil
}

// The browse reconcile ran out of its time budget before finishing, nothing written
type BrowseBudgetExceeded struct{ Msg string }

func (e *BrowseBudgetExceeded) Error() string { return e.Msg }

func (e *BrowseBudgetExceeded) PyTypeName() string { return "BrowseBudgetExceeded" }

// One monotonic deadline for everything a browse does while holding gateway.lock
type browseBudget struct {
	deadline time.Time
	expired  bool
}

// Budget expiring the given seconds from now
func newBrowseBudget(seconds float64) *browseBudget {
	return &browseBudget{deadline: time.Now().Add(secondsOf(seconds))}
}

// Seconds left before the deadline, never negative
func (b *browseBudget) remaining() float64 { return max(time.Until(b.deadline).Seconds(), 0.0) }

// Busy timeout for a connection opened under the budget, at least fifty milliseconds
func (b *browseBudget) busyTimeout() float64 { return max(b.remaining(), 0.05) }

// Installs a progress handler interrupting the connection's statements once the deadline passes
func (b *browseBudget) arm(conn SQLiteConn) error {
	raw, ok := rawConnOf(conn)
	if !ok {
		return fmt.Errorf("connection %T exposes no driver connection for a progress handler", conn)
	}
	return pysqlite.SetProgressHandler(raw, func() int {
		if !time.Now().Before(b.deadline) {
			b.expired = true
			return 1
		}
		return 0
	}, budgetProgressOps)
}

// Fails once the budget is spent, latching the expiry
func (b *browseBudget) check() error {
	if b.expired || b.remaining() <= 0 {
		b.expired = true
		return &BrowseBudgetExceeded{"browse schema catch-up exceeded its time budget"}
	}
	return nil
}

// Adds the missing declared tables and columns in one immediate transaction, rolled back on any shortfall
func applyAdditiveSchema(ctx context.Context, path string, budget *browseBudget) error {
	if budget == nil {
		budget = newBrowseBudget(BrowseReconcileBudgetS)
	}
	if err := budget.check(); err != nil {
		return err
	}
	conn, err := ConnectTracked(path, WithTrackingPath(path), WithTimeout(budget.busyTimeout()))
	if err != nil {
		return err
	}
	defer conn.Close()
	reapplyDurabilityBarriers(ctx, conn)
	if err := budget.arm(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	err = func() error {
		if err := ApplyDeclaredSchema(ctx, conn); err != nil {
			return err
		}
		statements, err := SchemaReadProbeStatements(ctx)
		if err != nil {
			return err
		}
		for _, statement := range statements {
			if _, err := fetchOne(ctx, conn, statement); err != nil {
				if isOperationalError(err) && schemaNotBuilt(err) {
					return &DeclarativeDriftError{Detail: errText(err)}
				}
				return err
			}
		}
		if err := budget.check(); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, "COMMIT")
		return err
	}()
	if err != nil {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		if budget.expired && isOperationalError(err) {
			return &BrowseBudgetExceeded{"browse schema catch-up exceeded its time budget"}
		}
		return err
	}
	return nil
}

// Lagging store that is also damaged, worded like the malformed image error so callers map it to corrupt
type CorruptStoreError struct{ Detail string }

func (e *CorruptStoreError) Error() string {
	return "database disk image is malformed (integrity_check: " + e.Detail + ")"
}

func (e *CorruptStoreError) PyTypeName() string { return "DatabaseError" }

// First integrity_check problems of a store under the budget, found false when the scan answers ok
func integrityProblem(ctx context.Context, path string, budget *browseBudget) (string, bool, error) {
	if err := budget.check(); err != nil {
		return "", false, err
	}
	timeout := budget.busyTimeout()
	var rows []*pyjson.Object
	conn, err := connectReadOnlyTracked(path, &timeout)
	if err == nil {
		err = func() error {
			defer conn.Close()
			if err := budget.arm(conn); err != nil {
				return err
			}
			var qerr error
			rows, qerr = queryObjects(ctx, conn, "PRAGMA integrity_check(3)")
			return qerr
		}()
	}
	if err != nil {
		if isDatabaseError(err) {
			if budget.expired {
				return "", false, &BrowseBudgetExceeded{"integrity check exceeded the browse time budget"}
			}
			if IsMalformedDBError(err) {
				return errText(err), true, nil
			}
		}
		return "", false, err
	}
	if err := budget.check(); err != nil {
		return "", false, err
	}
	problems := []string{}
	for _, row := range rows {
		if len(row.Keys) == 0 {
			continue
		}
		text := pyStr(row.Vals[row.Keys[0]])
		if strings.ToLower(text) != "ok" {
			problems = append(problems, text)
		}
	}
	if len(problems) == 0 {
		return "", false, nil
	}
	return strings.Join(problems[:min(3, len(problems))], "; "), true, nil
}

// Size and modification stamp of one store file, absent when it does not exist
type fileStamp struct {
	size, mtimeNS int64
	present       bool
}

// Stamps of the main file and its WAL, any committed write changing one of them
type storeStamp [2]fileStamp

// Signature of a store's main file and WAL
func storeSignature(path string) (storeStamp, error) {
	var out storeStamp
	for i, p := range []string{path, path + "-wal"} {
		st, err := os.Stat(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return out, err
		}
		out[i] = fileStamp{size: st.Size(), mtimeNS: st.ModTime().UnixNano(), present: true}
	}
	return out, nil
}

var (
	scanGuard   sync.Mutex
	scanLocks   = map[string]*sync.Mutex{}
	scanTooSlow = map[string]storeStamp{}
)

// Process wide lock serializing integrity scans of one store
func pathScanLock(path string) (*sync.Mutex, error) {
	key, err := pyRealpath(path)
	if err != nil {
		return nil, err
	}
	scanGuard.Lock()
	defer scanGuard.Unlock()
	lock, ok := scanLocks[key]
	if !ok {
		lock = &sync.Mutex{}
		scanLocks[key] = lock
	}
	return lock, nil
}

// Another browse or maintenance holder kept gateway.lock past its budget, carrying the pending drift
type siblingStillBusy struct{ Drift string }

func (e *siblingStillBusy) Error() string { return e.Drift }

// Reserves a home for a browse, an unusable gateway.lock refused by name with its recovery text
func reserving(ctx context.Context, home string) (func(), error) {
	_, release, err := ExclusiveMaintenance(ctx, []string{home}, BrowseReconcilePurpose)
	if err != nil {
		if IsOwnershipConflict(err) {
			return nil, err
		}
		if isOSError(err) {
			return nil, &DeclarativeDriftError{Detail: fmt.Sprintf("Session store is behind this Hermes version, but its "+
				"gateway.lock cannot be used (%s); nothing was changed.", pyOSErrorStr(err))}
		}
		return nil, err
	}
	return release, nil
}

// Holds gateway.lock for the DDL, retrying a sibling holder within its budget, current true when nothing is left to do
func reserveForDDL(ctx context.Context, home, path string, owned func(drift string) error) (bool, func(), error) {
	deadline := time.Now().Add(secondsOf(BrowseReconcileBudgetS + 0.5))
	release := func() {}
	for {
		rel, err := reserving(ctx, home)
		if err == nil {
			release = rel
			break
		}
		if !IsOwnershipConflict(err) {
			return false, nil, err
		}
		pending, drift, derr := SchemaDriftReason(ctx, path)
		if derr != nil {
			return false, nil, derr
		}
		if !drift {
			break
		}
		if _, live, lerr := LiveRuntimeOwner(home); lerr != nil {
			return false, nil, lerr
		} else if live {
			return false, nil, owned(pending)
		}
		if !time.Now().Before(deadline) {
			return false, nil, &siblingStillBusy{Drift: pending}
		}
		time.Sleep(secondsOf(browseSiblingPollS))
	}
	_, drift, err := SchemaDriftReason(ctx, path)
	if err != nil {
		release()
		return false, nil, err
	}
	return !drift, release, nil
}

// Full read only integrity scan outside gateway.lock, returning the signature it vouches for
func verifyCleanBeforeLock(ctx context.Context, path string, tooSlow func() error) (storeStamp, error) {
	var none storeStamp
	if StorageState(path) == StorageCorrupt {
		reason, ok := StorageCorruptReason(path)
		if !ok || reason == "" {
			reason = "latched corrupt"
		}
		return none, &CorruptStoreError{Detail: reason}
	}
	key, err := pyRealpath(path)
	if err != nil {
		return none, err
	}
	signature, err := storeSignature(path)
	if err != nil {
		return none, err
	}
	scanGuard.Lock()
	slow, seen := scanTooSlow[key]
	scanGuard.Unlock()
	if seen && slow == signature {
		return none, tooSlow()
	}
	problem, found, err := integrityProblem(ctx, path, newBrowseBudget(BrowseIntegrityBudgetS))
	if err != nil {
		var exceeded *BrowseBudgetExceeded
		if errors.As(err, &exceeded) {
			scanGuard.Lock()
			scanTooSlow[key] = signature
			scanGuard.Unlock()
			return none, tooSlow()
		}
		return none, err
	}
	if found {
		MarkStorageCorrupt(path, problem)
		return none, &CorruptStoreError{Detail: problem}
	}
	return signature, nil
}

// Catches a lagging store up for a read only browse, true when DDL ran and false when already current
func ReconcileDriftForBrowse(ctx context.Context, path string) (bool, error) {
	requireOwnershipHooks()
	reason, drift, err := SchemaDriftReason(ctx, path)
	if err != nil || !drift {
		return false, err
	}
	conn, err := connectReadOnlyTracked(path, nil)
	if err != nil {
		return false, err
	}
	err = func() error {
		defer conn.Close()
		return requireHistoryTables(ctx, conn)
	}()
	if err != nil {
		var conv *pyConvError
		if errors.As(err, &conv) && conv.Class == "RuntimeError" {
			return false, &DeclarativeDriftError{Detail: fmt.Sprintf("Session store is missing its history tables (%s); "+
				"this is not upgrade drift. Run `icebath sessions recover`.", reason)}
		}
		return false, err
	}
	home := filepath.Dir(path)
	label, _ := ownerRemedy(home)
	owned := func(drift, holder string) error {
		restart := "icebath gateway restart"
		if holder == "" {
			var err error
			if restart, err = owningGatewayRestart(home); err != nil {
				return err
			}
		}
		who := ""
		if holder != "" {
			who = " (" + holder + ")"
		}
		return &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' is behind this Hermes version (%s), and a "+
			"running gateway owns it%s, so browsing will not write to it. Restart that gateway "+
			"(`%s`) to bring the store up to date.", label, drift, who, restart), Owned: true}
	}
	tooSlow := func() error {
		if _, live, err := LiveRuntimeOwner(home); err != nil {
			return err
		} else if live {
			return owned(reason, "")
		}
		return &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' is behind this Hermes version (%s) and is too "+
			"large to verify and update within %ss while browsing. Your "+
			"history is untouched: nothing was changed and the list was not cleared. Run "+
			"`%s` to bring it up to date (it has no time limit), then reload.", label, reason, pyFixed0(BrowseReconcileBudgetS), repairCommand(home))}
	}
	if release, err := reserving(ctx, home); err == nil {
		release()
	} else if IsOwnershipConflict(err) {
		if _, live, lerr := LiveRuntimeOwner(home); lerr != nil {
			return false, lerr
		} else if live {
			return false, owned(reason, "")
		}
	} else {
		return false, err
	}
	if holder, found, err := locklessGatewayHolder(path); err != nil {
		return false, err
	} else if found {
		return false, owned(reason, holder)
	}
	scanLock, err := pathScanLock(path)
	if err != nil {
		return false, err
	}
	ran, err := func() (bool, error) {
		scanLock.Lock()
		defer scanLock.Unlock()
		if _, drift, err := SchemaDriftReason(ctx, path); err != nil || !drift {
			return false, err
		}
		signature, err := verifyCleanBeforeLock(ctx, path, tooSlow)
		if err != nil {
			return false, err
		}
		err = func() error {
			current, release, err := reserveForDDL(ctx, home, path, func(drift string) error { return owned(drift, "") })
			if err != nil {
				return err
			}
			defer release()
			if current {
				return errAlreadyCurrent
			}
			budget := newBrowseBudget(BrowseReconcileBudgetS)
			if holder, found, err := locklessGatewayHolder(path); err != nil {
				return err
			} else if found {
				return owned(reason, holder)
			}
			now, err := storeSignature(path)
			if err != nil {
				return err
			}
			if now != signature {
				return &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' changed while browsing verified it "+
					"(%s); nothing was changed. Retry in a moment; the list was not cleared.", label, reason)}
			}
			notWritable := func(why string) error {
				return &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' is behind this Hermes version (%s), "+
					"but %s is not writable by this user, so browsing cannot add the missing "+
					"tables/columns; nothing was changed. Fix the permissions, then reload.", label, reason, why)}
			}
			for _, target := range []string{path, filepath.Dir(path)} {
				if !osAccess(target, 2) {
					return notWritable(target)
				}
			}
			if err := applyAdditiveSchema(ctx, path, budget); err != nil {
				var driftErr *DeclarativeDriftError
				if isOperationalError(err) && strings.Contains(strings.ToLower(errText(err)), "readonly") {
					return notWritable(path)
				}
				if errors.As(err, &driftErr) {
					return &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' is behind this Hermes version and could not "+
						"be caught up (%s); nothing was changed. Run `icebath sessions repair` for that "+
						"profile.", label, driftErr.Detail)}
				}
				return err
			}
			return nil
		}()
		if errors.Is(err, errAlreadyCurrent) {
			return false, nil
		}
		var exceeded *BrowseBudgetExceeded
		var sibling *siblingStillBusy
		if errors.As(err, &exceeded) {
			return false, tooSlow()
		}
		if errors.As(err, &sibling) {
			return false, &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' is being brought up to date by another Hermes "+
				"process (%s). Retry in a moment; the list was not cleared.", label, sibling.Drift)}
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}()
	if err != nil || !ran {
		return false, err
	}
	if after, drift, err := SchemaDriftReason(ctx, path); err != nil {
		return false, err
	} else if drift {
		return false, &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' is behind this Hermes version and could not be "+
			"caught up (%s). Run `icebath sessions repair` for that profile.", label, after)}
	}
	slog.Warn(fmt.Sprintf("state.db at %s lagged SCHEMA_SQL (%s); added the missing tables/columns for browsing", path, reason))
	return true, nil
}

// Sentinel for a store a sibling caught up while this browse waited for the lock
var errAlreadyCurrent = errors.New("store already current")

// Explicit additive repair beneath the runtime owner lock and the repair lock, every refusal landing in the report
func RepairDeclarativeSchema(ctx context.Context, path string, backup bool) *DeclarativeRepairReport {
	requireOwnershipHooks()
	report := &DeclarativeRepairReport{}
	err := func() error {
		_, release, err := ExclusiveMaintenance(ctx, []string{filepath.Dir(path)}, "")
		if err != nil {
			return err
		}
		defer release()
		return crossProcessRepairLock(path, func(holding bool) error {
			if !holding {
				return runtimeError("could not obtain the state.db repair lock")
			}
			return reconcileLocked(ctx, path, backup, report)
		})
	}()
	if err != nil {
		report.Error = runtimeErrorText(err)
	}
	return report
}
