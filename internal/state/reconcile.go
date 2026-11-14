package state

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

const (
	BrowseReconcileBudget  = 3 * time.Second
	BrowseReconcilePurpose = "browse_schema_reconcile"
	browseIntegrityBudget  = BrowseReconcileBudget
	browseSiblingPoll      = 100 * time.Millisecond
	browseBusyFloor        = 50 * time.Millisecond
	browseSiblingSlack     = 500 * time.Millisecond
)

var ErrOwnershipConflict = errors.New("gateway runtime ownership conflict")

var ErrNoRuntimeOwnership = errors.New("no gateway runtime ownership hooks configured for schema reconciliation")

var errBrowseBudgetExceeded = errors.New("browse schema catch-up exceeded its time budget")

var errIntegrityBudgetExceeded = errors.New("integrity check exceeded the browse time budget")

type RuntimeOwnership struct {
	Reserve   func(ctx context.Context, home, purpose string) (func(), error)
	LiveOwner func(home string) bool
	LockHome  func(home string) (string, bool)
}

func (o RuntimeOwnership) ready() bool {
	return o.Reserve != nil && o.LiveOwner != nil && o.LockHome != nil
}

type DeclarativeDriftError struct {
	Detail string
	Owned  bool
}

func (e *DeclarativeDriftError) Error() string { return e.Detail }

type CorruptStoreError struct {
	Detail string
}

func (e *CorruptStoreError) Error() string {
	return "database disk image is malformed (integrity_check: " + e.Detail + ")"
}

func (e *CorruptStoreError) PersistenceCause() Cause { return CauseCorrupt }

type browseScans struct {
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	tooSlow map[string]storeSignature
}

func (b *browseScans) lockFor(key string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.locks == nil {
		b.locks = map[string]*sync.Mutex{}
	}
	if b.locks[key] == nil {
		b.locks[key] = &sync.Mutex{}
	}
	return b.locks[key]
}

func (b *browseScans) slow(key string, sig storeSignature) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	recorded, ok := b.tooSlow[key]
	return ok && recorded == sig
}

func (b *browseScans) markSlow(key string, sig storeSignature) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tooSlow == nil {
		b.tooSlow = map[string]storeSignature{}
	}
	b.tooSlow[key] = sig
}

func IsDeclarativeDrift(err error) bool { return sqliteOperational(err) && schemaNotBuilt(err) }

func probeStatement(ctx context.Context, q Conn, statement string) error {
	rows, err := q.QueryContext(ctx, statement)
	if err != nil {
		return err
	}
	rows.Next()
	return rowsDone(rows)
}

func (e *Engine) SchemaDriftReason(ctx context.Context, path string) (string, error) {
	c, err := e.connect(ctx, ReadOnlyURI(path), path)
	if err != nil {
		return "", presentable(err)
	}
	defer func() { _ = c.close() }()
	statements, err := SchemaReadProbeStatements()
	if err != nil {
		return "", err
	}
	for _, statement := range statements {
		if err := probeStatement(ctx, c.q(), statement); err != nil {
			err = presentable(err)
			if sqliteOperational(err) && schemaNotBuilt(err) {
				return errText(err), nil
			}
			return "", err
		}
	}
	return "", nil
}

func requireHistoryTables(ctx context.Context, q Conn) error {
	tables, err := queryStrings(ctx, q, "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return err
	}
	if !containsString(tables, "sessions") || !containsString(tables, "messages") {
		return errors.New("canonical history tables are missing; use sessions recover, not additive reconciliation")
	}
	return nil
}

func (e *Engine) requireHistoryTablesReadOnly(ctx context.Context, path string) error {
	c, err := e.connect(ctx, ReadOnlyURI(path), path)
	if err != nil {
		return presentable(err)
	}
	defer func() { _ = c.close() }()
	return requireHistoryTables(ctx, c.q())
}

func (e *Engine) reconcileSnapshot(ctx context.Context, path string, guard *conn) (err error) {
	scratch := path + ".reconcile-scratch"
	if problem := repairScratchSpaceError(path); problem != "" {
		return errors.New(problem)
	}
	if problem := unlinkDBTriple(scratch); problem != "" {
		return errors.New(problem)
	}
	defer func() {
		if problem := unlinkDBTriple(scratch); problem != "" {
			slog.Warn("Could not remove reconciliation snapshot: " + problem)
		}
	}()
	if err := e.copyDatabaseSnapshot(ctx, path, scratch, guard, nil); err != nil {
		return err
	}
	if err := func() error {
		c, err := e.connectRepairDurable(ctx, scratch, repairConnTimeout)
		if err != nil {
			return err
		}
		defer func() { _ = c.close() }()
		if err := requireHistoryTables(ctx, c.q()); err != nil {
			return err
		}
		return ApplyDeclaredSchema(ctx, c.q())
	}(); err != nil {
		return err
	}
	reason, err := e.SchemaDriftReason(ctx, scratch)
	if err != nil {
		return err
	}
	if reason != "" {
		return errors.New("schema reconciliation incomplete: " + reason)
	}
	if reason, err = e.DBOpensCleanly(ctx, scratch); err != nil {
		return err
	}
	if reason != "" {
		return errors.New("reconciled snapshot is unhealthy: " + reason)
	}
	return e.copyDatabaseSnapshot(ctx, scratch, path, nil, guard)
}

func (e *Engine) reconcileLocked(ctx context.Context, path string, noBackup bool, report *RepairReport) error {
	writer, err := e.LiveWriterHoldsDB(ctx, path)
	if err != nil {
		return err
	}
	if writer {
		return errors.New("a live writer still holds state.db; stop it and retry")
	}
	if err := e.requireHistoryTablesReadOnly(ctx, path); err != nil {
		return err
	}
	drift, err := e.SchemaDriftReason(ctx, path)
	if err != nil {
		return err
	}
	if drift == "" {
		report.Repaired, report.Strategy = true, "already_healthy"
		return nil
	}
	reason, err := e.DBOpensCleanly(ctx, path)
	if err != nil {
		return err
	}
	if reason != "" {
		return errors.New(reason)
	}
	if !noBackup {
		backup, refusal := e.backupDBFile(path)
		if backup == "" {
			return errors.New("pre-repair backup refused: " + refusal)
		}
		report.BackupPath = backup
	}
	guard, guardErr := e.openExclusive(ctx, path, "BEGIN EXCLUSIVE")
	if guardErr != nil {
		return fmt.Errorf("could not acquire exclusive state.db repair ownership: %s", errText(guardErr))
	}
	defer func() { _ = closeUnpinned(ctx, guard) }()
	if err := e.reconcileSnapshot(ctx, path, guard); err != nil {
		return err
	}
	report.Repaired, report.Strategy = true, "reconcile_declarative_schema"
	return nil
}

type ReconcileOptions struct {
	NoBackup  bool
	Ownership RuntimeOwnership
}

func (e *Engine) RepairDeclarativeSchema(ctx context.Context, path string, o ReconcileOptions) (RepairReport, error) {
	if !o.Ownership.ready() {
		return RepairReport{}, ErrNoRuntimeOwnership
	}
	var report RepairReport
	err := func() error {
		release, err := o.Ownership.Reserve(ctx, filepath.Dir(path), "")
		if err != nil {
			return err
		}
		defer release()
		unlock, held := e.crossProcessRepairLock(ctx, path)
		defer unlock()
		if !held {
			return errors.New("could not obtain the state.db repair lock")
		}
		return e.reconcileLocked(ctx, path, o.NoBackup, &report)
	}()
	if err != nil {
		report.Error = errText(err)
	}
	return report, nil
}

type browseBudget struct {
	deadline time.Time
	expired  bool
}

func newBrowseBudget(d time.Duration) *browseBudget {
	return &browseBudget{deadline: time.Now().Add(d)}
}

func (b *browseBudget) remaining() time.Duration { return max(time.Until(b.deadline), 0) }

func (b *browseBudget) busyTimeout() time.Duration { return max(b.remaining(), browseBusyFloor) }

func (b *browseBudget) arm(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, b.deadline)
}

func (b *browseBudget) observe(armed context.Context) {
	if errors.Is(armed.Err(), context.DeadlineExceeded) && !time.Now().Before(b.deadline) {
		b.expired = true
	}
}

func (b *browseBudget) check() error {
	if b.expired || b.remaining() <= 0 {
		b.expired = true
		return errBrowseBudgetExceeded
	}
	return nil
}

func (e *Engine) connectBudgeted(ctx context.Context, dsn, path string, budget *browseBudget) (*conn, error) {
	c, err := e.connect(ctx, dsn, path)
	if err != nil {
		return nil, presentable(err)
	}
	if _, err := c.q().ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", budget.busyTimeout().Milliseconds())); err != nil {
		return nil, withCleanup(presentable(err), c.close())
	}
	return c, nil
}

func (e *Engine) applyAdditiveSchema(ctx context.Context, path string, budget *browseBudget) error {
	if err := budget.check(); err != nil {
		return err
	}
	c, err := e.connectBudgeted(ctx, fileURI(path), path, budget)
	if err != nil {
		return err
	}
	defer func() { _ = c.close() }()
	q := c.q()
	reapplyDurabilityBarriers(ctx, q)
	armed, cancel := budget.arm(ctx)
	defer cancel()
	if _, err := q.ExecContext(armed, "BEGIN IMMEDIATE"); err != nil {
		budget.observe(armed)
		return presentable(err)
	}
	err = func() error {
		if err := ApplyDeclaredSchema(armed, q); err != nil {
			return err
		}
		statements, err := SchemaReadProbeStatements()
		if err != nil {
			return err
		}
		for _, statement := range statements {
			if err := probeStatement(armed, q, statement); err != nil {
				err = presentable(err)
				if sqliteOperational(err) && schemaNotBuilt(err) {
					return &DeclarativeDriftError{Detail: errText(err)}
				}
				return err
			}
		}
		if err := budget.check(); err != nil {
			return err
		}
		_, err = q.ExecContext(armed, "COMMIT")
		return presentable(err)
	}()
	if err == nil {
		return nil
	}
	budget.observe(armed)
	_, _ = q.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	if budget.expired && (sqliteOperational(err) || errors.Is(err, context.DeadlineExceeded)) {
		return errBrowseBudgetExceeded
	}
	return err
}

func (e *Engine) integrityProblem(ctx context.Context, path string, budget *browseBudget) (string, error) {
	if err := budget.check(); err != nil {
		return "", errIntegrityBudgetExceeded
	}
	lines, err := func() ([]string, error) {
		c, err := e.connectBudgeted(ctx, ReadOnlyURI(path), path, budget)
		if err != nil {
			return nil, err
		}
		defer func() { _ = c.close() }()
		armed, cancel := budget.arm(ctx)
		defer cancel()
		lines, err := queryStrings(armed, c.q(), "PRAGMA integrity_check(3)")
		if err != nil {
			budget.observe(armed)
			return nil, presentable(err)
		}
		return lines, nil
	}()
	if err != nil {
		if budget.expired {
			return "", errIntegrityBudgetExceeded
		}
		if sqliteFailure(err) && IsMalformed(err) {
			return errText(err), nil
		}
		return "", err
	}
	if err := budget.check(); err != nil {
		return "", errIntegrityBudgetExceeded
	}
	var problems []string
	for _, line := range lines {
		if strings.ToLower(line) != "ok" {
			problems = append(problems, line)
		}
	}
	return strings.Join(problems[:min(3, len(problems))], "; "), nil
}

type sigEntry struct {
	present bool
	size    int64
	mtime   int64
}

type storeSignature [2]sigEntry

func storeSignatureOf(path string) (storeSignature, error) {
	var sig storeSignature
	for i, p := range []string{path, path + "-wal"} {
		st, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return sig, err
		}
		sig[i] = sigEntry{true, st.Size(), st.ModTime().UnixNano()}
	}
	return sig, nil
}

func profileLabel(homeDir string) string {
	name, ok := home.ProfileName(homeDir)
	if !ok {
		return ""
	}
	return name
}

func repairCommand(homeDir string) string {
	switch label := profileLabel(homeDir); label {
	case home.DefaultProfile:
		return "icebath sessions repair"
	case "":
		return home.EnvHome + "=" + homeDir + " icebath sessions repair"
	default:
		return "icebath -p " + label + " sessions repair"
	}
}

func ownerRemedy(homeDir string) (string, string) {
	label := profileLabel(homeDir)
	if label != "" && label != home.DefaultProfile {
		return label, "icebath -p " + label + " gateway restart"
	}
	if label == "" {
		label = homeDir
	}
	return label, "icebath gateway restart"
}

func canonicalHome(p string) string { return normCase(resolvePath(home.ExpandUser(p))) }

func owningGatewayRestart(o RuntimeOwnership, homeDir string) string {
	if owner, ok := o.LockHome(homeDir); ok && owner != "" && canonicalHome(owner) != canonicalHome(homeDir) {
		_, restart := ownerRemedy(owner)
		return restart
	}
	_, restart := ownerRemedy(homeDir)
	return restart
}

func locklessGatewayHolder(path string) string {
	for _, h := range ForeignHolders(path) {
		if h.PID <= 0 {
			continue
		}
		argv := holderArgv(h.PID)
		if len(argv) > 0 && looksLikeProduct(argv) && containsString(argv[1:], "gateway") {
			return DescribeHolderPID(h.PID)
		}
	}
	return ""
}

func reserving(ctx context.Context, o RuntimeOwnership, homeDir string) (func(), error) {
	release, err := o.Reserve(ctx, homeDir, BrowseReconcilePurpose)
	if err == nil {
		return release, nil
	}
	if errors.Is(err, ErrOwnershipConflict) {
		return nil, err
	}
	return nil, &DeclarativeDriftError{Detail: fmt.Sprintf("Session store is behind this icebath version, but its "+
		"gateway.lock cannot be used (%s); nothing was changed.", errText(err))}
}

type siblingStillBusy struct{ drift string }

func (s *siblingStillBusy) Error() string { return s.drift }

func (e *Engine) reserveForDDL(ctx context.Context, o RuntimeOwnership, homeDir, path string, owned func(drift, holder string) error, body func(current bool) error) error {
	deadline := time.Now().Add(BrowseReconcileBudget + browseSiblingSlack)
	release := func() {}
	for {
		rel, err := reserving(ctx, o, homeDir)
		if err == nil {
			release = rel
			break
		}
		if !errors.Is(err, ErrOwnershipConflict) {
			return err
		}
		pending, derr := e.SchemaDriftReason(ctx, path)
		if derr != nil {
			return derr
		}
		if pending == "" {
			break
		}
		if o.LiveOwner(homeDir) {
			return owned(pending, "")
		}
		if !time.Now().Before(deadline) {
			return &siblingStillBusy{drift: pending}
		}
		if err := sleepCtx(ctx, browseSiblingPoll); err != nil {
			return err
		}
	}
	defer release()
	after, err := e.SchemaDriftReason(ctx, path)
	if err != nil {
		return err
	}
	return body(after == "")
}

func (e *Engine) verifyCleanBeforeLock(ctx context.Context, path string, tooSlow func() error) (storeSignature, error) {
	if StorageHealth(path) == HealthCorrupt {
		reason, _ := CorruptReason(path)
		if reason == "" {
			reason = "latched corrupt"
		}
		return storeSignature{}, &CorruptStoreError{Detail: reason}
	}
	key := resolvePath(path)
	sig, err := storeSignatureOf(path)
	if err != nil {
		return sig, err
	}
	if e.browse.slow(key, sig) {
		return sig, tooSlow()
	}
	problem, err := e.integrityProblem(ctx, path, newBrowseBudget(browseIntegrityBudget))
	if errors.Is(err, errIntegrityBudgetExceeded) {
		e.browse.markSlow(key, sig)
		return sig, tooSlow()
	}
	if err != nil {
		return sig, err
	}
	if problem != "" {
		MarkCorrupt(path, errors.New(problem))
		return sig, &CorruptStoreError{Detail: problem}
	}
	return sig, nil
}

func (e *Engine) ReconcileDriftForBrowse(ctx context.Context, path string, o RuntimeOwnership) (bool, error) {
	if !o.ready() {
		return false, ErrNoRuntimeOwnership
	}
	reason, err := e.SchemaDriftReason(ctx, path)
	if err != nil || reason == "" {
		return false, err
	}
	if err := e.requireHistoryTablesReadOnly(ctx, path); err != nil {
		if sqliteFailure(err) {
			return false, err
		}
		return false, &DeclarativeDriftError{Detail: fmt.Sprintf("Session store is missing its history tables (%s); this "+
			"is not upgrade drift. Run `icebath sessions recover`.", reason)}
	}
	homeDir := filepath.Dir(path)
	label, _ := ownerRemedy(homeDir)
	owned := func(drift, holder string) error {
		restart, who := owningGatewayRestart(o, homeDir), ""
		if holder != "" {
			restart, who = "icebath gateway restart", " ("+holder+")"
		}
		return &DeclarativeDriftError{Owned: true, Detail: fmt.Sprintf("Session store for profile '%s' is behind this "+
			"icebath version (%s), and a running gateway owns it%s, so browsing will not write to it. Restart that "+
			"gateway (`%s`) to bring the store up to date.", label, drift, who, restart)}
	}
	tooSlow := func() error {
		if o.LiveOwner(homeDir) {
			return owned(reason, "")
		}
		return &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' is behind this icebath version "+
			"(%s) and is too large to verify and update within %.0fs while browsing. Your history is untouched: nothing "+
			"was changed and the list was not cleared. Run `%s` to bring it up to date (it has no time limit), then reload.",
			label, reason, BrowseReconcileBudget.Seconds(), repairCommand(homeDir))}
	}
	release, err := reserving(ctx, o, homeDir)
	switch {
	case err == nil:
		release()
	case errors.Is(err, ErrOwnershipConflict):
		if o.LiveOwner(homeDir) {
			return false, owned(reason, "")
		}
	default:
		return false, err
	}
	if holder := locklessGatewayHolder(path); holder != "" {
		return false, owned(reason, holder)
	}
	scan := e.browse.lockFor(resolvePath(path))
	ran, err := func() (bool, error) {
		scan.Lock()
		defer scan.Unlock()
		current, err := e.SchemaDriftReason(ctx, path)
		if err != nil || current == "" {
			return false, err
		}
		signature, err := e.verifyCleanBeforeLock(ctx, path, tooSlow)
		if err != nil {
			return false, err
		}
		ran := false
		err = e.reserveForDDL(ctx, o, homeDir, path, owned, func(current bool) error {
			if current {
				return nil
			}
			budget := newBrowseBudget(BrowseReconcileBudget)
			if holder := locklessGatewayHolder(path); holder != "" {
				return owned(reason, holder)
			}
			now, err := storeSignatureOf(path)
			if err != nil {
				return err
			}
			if now != signature {
				return &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' changed while browsing "+
					"verified it (%s); nothing was changed. Retry in a moment; the list was not cleared.", label, reason)}
			}
			notWritable := func(why string) error {
				return &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' is behind this icebath "+
					"version (%s), but %s is not writable by this user, so browsing cannot add the missing tables/columns; "+
					"nothing was changed. Fix the permissions, then reload.", label, reason, why)}
			}
			for _, target := range []string{path, homeDir} {
				if !fileutil.Accessible(target, fileutil.AccessWrite) {
					return notWritable(target)
				}
			}
			err = e.applyAdditiveSchema(ctx, path, budget)
			var drift *DeclarativeDriftError
			switch {
			case err == nil:
				ran = true
				return nil
			case errors.Is(err, errBrowseBudgetExceeded):
				return err
			case errors.As(err, &drift):
				return &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' is behind this icebath "+
					"version and could not be caught up (%s); nothing was changed. Run `icebath sessions repair` for that "+
					"profile.", label, drift.Detail)}
			case sqliteOperational(err) && strings.Contains(lowerText(err), "readonly"):
				return notWritable(path)
			}
			return err
		})
		var busy *siblingStillBusy
		switch {
		case errors.Is(err, errBrowseBudgetExceeded):
			return false, tooSlow()
		case errors.As(err, &busy):
			return false, &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' is being brought up "+
				"to date by another icebath process (%s). Retry in a moment; the list was not cleared.", label, busy.drift)}
		}
		return ran, err
	}()
	if err != nil || !ran {
		return false, err
	}
	after, err := e.SchemaDriftReason(ctx, path)
	if err != nil {
		return false, err
	}
	if after != "" {
		return false, &DeclarativeDriftError{Detail: fmt.Sprintf("Session store for profile '%s' is behind this icebath "+
			"version and could not be caught up (%s). Run `icebath sessions repair` for that profile.", label, after)}
	}
	slog.Warn(fmt.Sprintf("state.db at %s lagged the declared schema (%s); added the missing tables/columns for browsing", path, reason))
	return true, nil
}
