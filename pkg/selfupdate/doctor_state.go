package selfupdate

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"gopkg.in/yaml.v3"
)

// doctorStatRow is one rendered state.db stats row, kind info or warn
type doctorStatRow struct {
	Kind, Text, Detail string
}

var (
	doctorLiveWriterHoldsDB   = state.LiveWriterHoldsStateDB
	doctorDBOpensCleanly      = state.DBOpensCleanly
	doctorStructuralDamage    = state.StateDBHasStructuralDamage
	doctorRepairStateDBSchema = func(ctx context.Context, dbPath string) (*state.RepairReport, error) {
		return state.RepairStateDBSchema(ctx, dbPath, true)
	}
	doctorExclusiveRepairDBGuard            = state.ExclusiveRepairDBGuard
	doctorCollectStateDBStats               = state.CollectStateDBStats
	doctorCountDBHolders                    = state.CountDBHolders
	doctorDeletedSidecarHolders             = state.IterDeletedSQLiteSidecarHolders
	doctorCronProbeReport                   = cron.ProbeReport
	doctorCronFreeBytes                     = cron.FreeBytes
	doctorDuplicateCredentialFindings       = gatewayrun.DuplicateCredentialFindings
	doctorListProfiles                      = func(ctx context.Context) ([]manage.ProfileInfo, error) { return manage.ListProfiles(ctx, false) }
	doctorLoadMemoryProvider                = memory.LoadMemoryProvider
	doctorCheckpointFootprintNotice         = tools.CheckpointFootprintNotice
	doctorPluginsInBothLists                = plugins.PluginsInBothLists
	doctorWriteProbeSnapshotMaxBytes        = int64(1 << 30)
	doctorUnprunedCacheWarnBytes            = int64(1 << 30)
	doctorPrunedCacheDirs                   = map[string]bool{"scratch": true, "terminal": true}
	doctorMemoryProviderChecks              = map[string]func(ctx context.Context, d *Doctor, issues *[]string) error{}
	doctorOrphanWrapperPattern              = regexp.MustCompile(`icebath -p (\S+)`)
	doctorStateDBSizeWarnBytes        int64 = 1 * 1024 * 1024 * 1024
)

// Plugin provenance of a plugins dir, the error an unreadable directory tree raises
var doctorPluginsProvenance = func(ctx context.Context, pluginsDir string) ([]plugins.Provenance, error) {
	return plugins.PluginsProvenance(ctx, pluginsDir), nil
}

// Effective memory section of a home config, empty when the config is absent or cannot be read
func doctorMemoryConfig(ctx context.Context, icebathHome string) (section map[string]any) {
	defer func() {
		if recover() != nil {
			section = map[string]any{}
		}
	}()
	configPath := filepath.Join(icebathHome, "config.yaml")
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil || !exists {
		return map[string]any{}
	}
	cfg, err := config.LoadUserEffective(configPath, config.EnvRefLookup)
	if err != nil {
		return map[string]any{}
	}
	if memorySection, ok := cfg["memory"].(map[string]any); ok {
		return memorySection
	}
	return map[string]any{}
}

// Value of a key in a mapping decoded as an ordered object or a plain map, false when absent
func doctorDictGet(mapping any, key string) (any, bool) {
	switch m := mapping.(type) {
	case *pyjson.Object:
		if m == nil {
			return nil, false
		}
		return m.Get(key)
	case map[string]any:
		v, ok := m[key]
		return v, ok
	}
	return nil, false
}

// Python truth value of a decoded value, an empty ordered object being false
func doctorTruthy(v any) bool {
	if o, ok := v.(*pyjson.Object); ok {
		return o != nil && o.Len() > 0
	}
	return config.PyTruthy(v)
}

// Float value of a Python number, false for anything else
func doctorFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	case *big.Int:
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, true
	}
	return 0, false
}

// Byte count as B, KB, MB, GB or TB with one decimal above bytes, a question mark otherwise
func doctorHumanBytes(n any) string {
	size, ok := doctorFloat(n)
	if !ok {
		return "?"
	}
	if size < 1024 {
		return fmt.Sprintf("%d B", int64(size))
	}
	for _, unit := range []string{"KB", "MB", "GB"} {
		size /= 1024.0
		if size < 1024 {
			return fmt.Sprintf("%.1f %s", size, unit)
		}
	}
	return fmt.Sprintf("%.1f TB", size/1024.0)
}

// Number with thousands separators the way the comma format spec renders it
func doctorCommaNumber(v any) string {
	text := pyrepr.Str(v)
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	whole, frac, hasFrac := strings.Cut(text, ".")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if hasFrac {
		return sign + b.String() + "." + frac
	}
	return sign + b.String()
}

// Note naming the one host gateway and its served profiles, empty when no gateway owns the host
func doctorHostGatewayNote() (note string) {
	defer func() {
		if recover() != nil {
			note = ""
		}
	}()
	topology, err := doctorHostGatewayTopology()
	if err != nil || topology == nil {
		return ""
	}
	return " (" + topology.Describe() + ")"
}

// Turns a state.db stats mapping into info and warn rows without any I/O
func doctorRenderStateDBStats(stats any, holders *int, hostNote string) []doctorStatRow {
	lines := []doctorStatRow{}
	get := func(key string) any {
		v, _ := doctorDictGet(stats, key)
		return v
	}
	logical, wal, freelist := get("logical_size_bytes"), get("wal_size_bytes"), get("freelist_count")
	sizeBits := []string{}
	if logical != nil {
		sizeBits = append(sizeBits, "logical size "+doctorHumanBytes(logical))
	}
	if v := get("page_count"); v != nil {
		sizeBits = append(sizeBits, doctorCommaNumber(v)+" pages")
	}
	if freelist != nil {
		sizeBits = append(sizeBits, doctorCommaNumber(freelist)+" free")
	}
	if wal != nil {
		sizeBits = append(sizeBits, "WAL "+doctorHumanBytes(wal))
	}
	if len(sizeBits) > 0 {
		lines = append(lines, doctorStatRow{"info", "state.db " + strings.Join(sizeBits, ", "), ""})
	}
	rowBits := []string{}
	if v := get("messages"); v != nil {
		rowBits = append(rowBits, doctorCommaNumber(v)+" messages")
	}
	if v := get("sessions"); v != nil {
		rowBits = append(rowBits, doctorCommaNumber(v)+" sessions")
	}
	if v := get("journal_mode"); doctorTruthy(v) {
		rowBits = append(rowBits, "journal_mode="+pyrepr.Str(v))
	}
	if holders != nil {
		rowBits = append(rowBits, fmt.Sprintf("%d process(es) holding the DB open%s", *holders, hostNote))
	}
	if len(rowBits) > 0 {
		lines = append(lines, doctorStatRow{"info", strings.Join(rowBits, ", "), ""})
	}
	if fts := get("fts_tables"); doctorTruthy(fts) {
		present := []string{}
		switch tables := fts.(type) {
		case *pyjson.Object:
			for _, name := range tables.Keys {
				if doctorTruthy(tables.Vals[name]) {
					present = append(present, name)
				}
			}
		case map[string]any:
			names := make([]string, 0, len(tables))
			for name := range tables {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if doctorTruthy(tables[name]) {
					present = append(present, name)
				}
			}
		}
		shown := "none"
		if len(present) > 0 {
			shown = strings.Join(present, ", ")
		}
		lines = append(lines, doctorStatRow{"info", "FTS tables: " + shown, ""})
	}
	deferral := get("fts_rebuild_deferral")
	_, isObject := deferral.(*pyjson.Object)
	_, isMap := deferral.(map[string]any)
	if isObject || isMap {
		orText := func(key, fallback string) string {
			if v, _ := doctorDictGet(deferral, key); doctorTruthy(v) {
				return pyrepr.Str(v)
			}
			return fallback
		}
		pids := orText("holder_pids", "unknown")
		if futile, _ := doctorDictGet(deferral, "futile"); doctorTruthy(futile) {
			lines = append(lines, doctorStatRow{"warn", "state.db FTS repair is blocked by the same holder(s) PID(s) " + pids + " for " +
				orText("holders_attempts", "?") + " consecutive deferral(s); waiting is futile",
				"(stop ONLY the listed process(es) — the host gateway keeps running and its own " +
					"retry rebuilds within a minute of the holder leaving)"})
		} else {
			lines = append(lines, doctorStatRow{"warn", "state.db FTS repair is blocked after " + orText("attempts", "?") + " deferral(s) " +
				"by PID(s) " + pids,
				"(stop the listed processes; the host gateway's own retry then rebuilds, or run " +
					"'icebath sessions optimize-storage' with every holder stopped)"})
		}
	}
	if size, ok := doctorFloat(logical); logical != nil && ok && size > float64(doctorStateDBSizeWarnBytes) {
		detail := "consider enabling sessions.auto_prune in config.yaml to bound growth"
		if doctorTruthy(get("fts_rebuild_pending")) || doctorTruthy(get("fts_teardown_pending")) || doctorTruthy(get("fts_storage_upgrade_needed")) {
			detail += "; run 'icebath sessions optimize-storage' offline (with the host gateway stopped) to compact FTS storage"
		}
		lines = append(lines, doctorStatRow{"warn", "state.db is large (" + doctorHumanBytes(logical) + ")", "(" + detail + ")"})
	}
	return lines
}

// Memory and user profile store flags the home config enables
func doctorMemoryStoreFlags(ctx context.Context, icebathHome string) (bool, bool) {
	return memory.BuiltinMemoryStoreFlags(doctorMemoryConfig(ctx, icebathHome))
}

// Reports the unused legacy checkout under an embedded desktop install with its size, never a deletion command
func doctorCheckLegacyDesktopCheckout(ctx context.Context, d *Doctor) {
	if steward := SealedSteward(d.ProjectRoot); steward == nil || *steward != StewardDesktop {
		return
	}
	checkout := filepath.Join(d.Home, "icebath-agent")
	if _, err := os.Stat(filepath.Join(checkout, ".git")); err != nil {
		return
	}
	doctorSection(d.W, "Legacy Desktop Checkout")
	git := func(args ...string) *pysubprocess.Completed {
		done, err := doctorSubprocessRun(ctx, append([]string{"git", "-C", checkout}, args...), pysubprocess.Options{Timeout: 10 * time.Second})
		if err != nil {
			return nil
		}
		return done
	}
	clean := func(done *pysubprocess.Completed, want string) bool {
		return done != nil && done.Returncode == 0 && pystr.Strip(string(done.Stdout)) == want
	}
	status := git("status", "--porcelain")
	branch := git("rev-parse", "--abbrev-ref", "HEAD")
	stashes := git("stash", "list")
	pristine := clean(status, "") && clean(branch, "main") && clean(stashes, "")
	sizeNote := ""
	var total int64
	walkErr := filepath.WalkDir(checkout, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if walkErr == nil {
		sizeNote = fmt.Sprintf(" (~%.1f GB)", float64(total)/1_000_000_000)
	}
	if pristine {
		doctorCheckWarn(d.W, "Unused checkout at "+d.DisplayHome+"/icebath-agent"+sizeNote,
			"(the desktop app runs embedded and does not use it; the tree is clean)")
		fmt.Fprintln(d.W, "    Review it and decide whether to keep or remove it — doctor does not delete anything.")
		return
	}
	doctorCheckInfo(d.W, "A checkout exists at "+d.DisplayHome+"/icebath-agent but holds local work "+
		"(changes, a branch, or stashes). The desktop app does not use "+
		"it; review it before you remove anything.")
}

// Text of a file decoded as UTF-8 with an optional signature
func doctorReadText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return pystr.DecodeUTF8SigFile(raw)
}

// Writes text the way a text mode file does, line feeds becoming CRLF on Windows
func doctorWriteText(path, text string) error {
	if runtime.GOOS == "windows" {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	return os.WriteFile(path, []byte(text), 0o666)
}

// Reports the home, its expected subdirectories, SOUL.md and the enabled built in memory files
func checkDirectoryStructure(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		func() {
			defer func() { _ = recover() }()
			doctorCheckLegacyDesktopCheckout(ctx, d)
		}()
		w, dhh, icebathHome := d.W, d.DisplayHome, d.Home
		if err := doctorEnsureDir(w, f, shouldFix, icebathHome, dhh+" directory exists", "Created "+dhh+" directory", dhh+" not found"); err != nil {
			return err
		}
		memoryEnabled, userProfileEnabled := doctorMemoryStoreFlags(ctx, icebathHome)
		memoryOn := memoryEnabled || userProfileEnabled
		subdirs := []string{"cron", "sessions", "logs", "skills"}
		if memoryOn {
			subdirs = append(subdirs, "memories")
		}
		for _, name := range subdirs {
			if err := doctorEnsureDir(w, f, shouldFix, filepath.Join(icebathHome, name), dhh+"/"+name+"/ exists",
				"Created "+dhh+"/"+name+"/", dhh+"/"+name+"/ not found"); err != nil {
				return err
			}
		}
		if err := doctorCheckScratchDir(ctx, w, icebathHome, dhh); err != nil {
			return err
		}
		soulPath := filepath.Join(icebathHome, "SOUL.md")
		if _, err := os.Stat(soulPath); err == nil {
			text, err := doctorReadText(soulPath)
			if err != nil {
				return err
			}
			configured := false
			for _, line := range pystr.Splitlines(pystr.Strip(text), false) {
				stripped := pystr.Strip(line)
				if stripped != "" && !strings.HasPrefix(stripped, "<!--") && !strings.HasPrefix(stripped, "-->") && !strings.HasPrefix(stripped, "#") {
					configured = true
					break
				}
			}
			if configured {
				doctorCheckOK(w, dhh+"/SOUL.md exists (persona configured)", "")
			} else {
				doctorCheckInfo(w, dhh+"/SOUL.md exists but is empty — edit it to customize personality")
			}
		} else {
			doctorCheckWarn(w, dhh+"/SOUL.md not found", "(create it to give Icebath a custom personality)")
			if shouldFix {
				if err := pyfs.Makedirs(filepath.Dir(soulPath), 0o777, true); err != nil {
					return err
				}
				if err := doctorWriteText(soulPath, "# Icebath Agent Persona\n\n<!-- Edit this file to customize how Icebath communicates. -->\n\n"+
					"You are Icebath, a helpful AI assistant.\n"); err != nil {
					return err
				}
				doctorCheckOK(w, "Created "+dhh+"/SOUL.md with basic template", "")
				f.Fixed++
			}
		}
		memoriesDir := filepath.Join(icebathHome, "memories")
		if !memoryOn {
			doctorCheckInfo(w, "Built-in memory files disabled by config")
			return nil
		}
		_, statErr := os.Stat(memoriesDir)
		existed := statErr == nil
		if err := doctorEnsureDir(w, f, shouldFix, memoriesDir, dhh+"/memories/ directory exists", "Created "+dhh+"/memories/",
			dhh+"/memories/ not found"); err != nil {
			return err
		}
		for _, store := range []struct {
			on   bool
			name string
		}{{memoryEnabled, "MEMORY.md"}, {userProfileEnabled, "USER.md"}} {
			if !store.on || !existed {
				continue
			}
			path := filepath.Join(memoriesDir, store.name)
			if _, err := os.Stat(path); err != nil {
				doctorCheckInfo(w, store.name+" not created yet (will be created when the agent first writes a memory)")
				continue
			}
			text, err := doctorReadText(path)
			if err != nil {
				return err
			}
			doctorCheckOK(w, fmt.Sprintf("%s exists (%d chars)", store.name, len([]rune(pystr.Strip(text)))), "")
		}
		return nil
	})
}

// doctorCacheHog is one cache root entry no pruner covers and its size
type doctorCacheHog struct {
	Name  string
	Bytes int64
}

// Cache root directories outside the pruned ones at or above a size, largest first
func doctorUnprunedCacheHogs(ctx context.Context, icebathHome string, minBytes int64) []doctorCacheHog {
	cache := filepath.Join(icebathHome, "cache")
	hogs := []doctorCacheHog{}
	entries, err := os.ReadDir(cache)
	if err != nil {
		return hogs
	}
	for _, entry := range entries {
		path := filepath.Join(cache, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || entry.Type()&fs.ModeSymlink != 0 || doctorPrunedCacheDirs[entry.Name()] {
			continue
		}
		if size := home.ScratchDirUsageBytes(ctx, path); size >= minBytes {
			hogs = append(hogs, doctorCacheHog{entry.Name(), size})
		}
	}
	sort.SliceStable(hogs, func(i, j int) bool { return hogs[i].Bytes > hogs[j].Bytes })
	return hogs
}

// Reports the scratch dir, its size, entries the prune skips, oversized unpruned cache dirs and a foreign TMPDIR
func doctorCheckScratchDir(ctx context.Context, w io.Writer, icebathHome, dhh string) error {
	scratch := home.ScratchDirWithoutPrune(ctx, icebathHome)
	size := doctorHumanBytes(home.ScratchDirUsageBytes(ctx, scratch))
	doctorCheckOK(w, fmt.Sprintf("%s/cache/scratch/ is the scratch dir (TMPDIR; %s, entries pruned after %vh idle)", dhh, size, home.ScratchMaxIdleHours), "")
	stuck, err := home.StuckEntries(scratch)
	if err != nil {
		return err
	}
	for _, entry := range stuck {
		doctorCheckWarn(w, dhh+"/cache/scratch/"+filepath.Base(entry)+"/ holds files you may not delete, so the prune skips it — "+home.RemovalHint(entry), "")
	}
	for _, hog := range doctorUnprunedCacheHogs(ctx, icebathHome, doctorUnprunedCacheWarnBytes) {
		doctorCheckWarn(w, dhh+"/cache/"+hog.Name+"/ is "+doctorHumanBytes(hog.Bytes)+" and outside every pruner "+
			"(only cache/scratch/ and cache/terminal/ are reaped) — move task files under "+
			"cache/scratch/<task>/ or delete it", "")
	}
	if tmpdir := os.Getenv("TMPDIR"); tmpdir != "" && tmpdir != os.Getenv(home.ScratchDirMarkerEnv) {
		doctorCheckInfo(w, "TMPDIR="+tmpdir+" is set by you or the OS, so Icebath leaves it alone")
	}
	return nil
}

// Session row count read through a read only connection
func doctorSessionCount(ctx context.Context, stateDBPath string) (int64, error) {
	uri, err := state.ReadOnlyDBURI(stateDBPath)
	if err != nil {
		return 0, err
	}
	conn, err := state.ConnectSQLite(uri, state.SQLiteConfig{Timeout: 5.0, URI: true})
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var count int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// Write health failure reason, probing a read only snapshot when a live writer holds the store
func doctorWriteHealthReason(ctx context.Context, w io.Writer, stateDBPath string, shouldFix bool) (string, bool, error) {
	held, err := doctorLiveWriterHoldsDB(ctx, stateDBPath)
	if err != nil {
		return "", false, err
	}
	if !held {
		return doctorDBOpensCleanly(ctx, stateDBPath)
	}
	if !shouldFix {
		info, err := os.Stat(stateDBPath)
		if err != nil {
			return "", false, err
		}
		if info.Size() > doctorWriteProbeSnapshotMaxBytes {
			doctorCheckInfo(w, "state.db write-health probe skipped: store is held by a live writer and larger than 1 GB "+
				"(run 'icebath doctor --fix' to probe it)")
			return "", false, nil
		}
	}
	tmp, err := os.MkdirTemp("", "tmp")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(tmp)
	snapshot := filepath.Join(tmp, "state.db")
	uri, err := state.ReadOnlyDBURI(stateDBPath)
	if err != nil {
		return "", false, err
	}
	src, err := state.ConnectSQLite(uri, state.SQLiteConfig{Timeout: 1.0, URI: true})
	if err != nil {
		return "", false, err
	}
	copyErr := func() error {
		defer src.Close()
		dest, err := state.ConnectSQLite(snapshot, state.SQLiteConfig{Timeout: 5.0})
		if err != nil {
			return err
		}
		defer dest.Close()
		return state.BackupSQLite(src, dest)
	}()
	if copyErr != nil {
		return "", false, copyErr
	}
	return doctorDBOpensCleanly(ctx, snapshot)
}

// doctorStateDBRepair is the wording of one repairable state.db corruption class
type doctorStateDBRepair struct {
	OKLabel, NotFixedLabel, FailedIssue, FixHint string
}

var doctorStateDBRepairs = map[string]doctorStateDBRepair{
	"fts": {"Repaired state.db FTS write health",
		"state.db FTS write-health repair did not recover automatically",
		"state.db FTS write corruption and auto-repair failed — restore from the backup copy beside state.db",
		"state.db FTS write corruption — run 'icebath doctor --fix' (or 'icebath sessions repair') to rebuild the FTS index"},
	"schema": {"Repaired state.db schema ({count} sessions recovered)",
		"state.db schema repair did not recover automatically",
		"state.db schema malformed and auto-repair failed — restore from the backup copy beside state.db",
		"state.db schema malformed — run 'icebath doctor --fix' (or 'icebath sessions repair') to recover hidden sessions"},
}

const doctorStateDBStructuralIssue = "state.db structural corruption (canonical tables/indexes damaged, not the FTS index) — an FTS rebuild " +
	"cannot repair it. Stop the gateway, then run 'icebath {profile_arg}sessions recover --source {db_path} " +
	"--inspect-only' and, if it reports recoverable, 'icebath {profile_arg}sessions recover --source {db_path} " +
	"--output recovered-state.db'. Do NOT restore a .malformed-backup copy beside state.db: it is a snapshot " +
	"of the same corrupt file."

// Text of a report field, None for an empty one
func doctorNoneText(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

// Records the fix hint or runs the repair of one state.db corruption class, structural damage going to manual issues
func doctorRepairStateDB(ctx context.Context, d *Doctor, f *Finding, shouldFix bool, stateDBPath, kind string) error {
	if kind == "structural" {
		issue := strings.NewReplacer("{profile_arg}", home.ProfileCLISelector(d.Home), "{db_path}", stateDBPath).Replace(doctorStateDBStructuralIssue)
		f.ManualIssues = append(f.ManualIssues, issue)
		return nil
	}
	repair := doctorStateDBRepairs[kind]
	if !shouldFix {
		f.Issues = append(f.Issues, repair.FixHint)
		return nil
	}
	report, err := doctorRepairStateDBSchema(ctx, stateDBPath)
	if err != nil {
		return err
	}
	if !report.Repaired {
		doctorCheckWarn(d.W, repair.NotFixedLabel, "("+doctorNoneText(report.Error)+"; backup: "+doctorNoneText(report.BackupPath)+")")
		f.Issues = append(f.Issues, repair.FailedIssue)
		return nil
	}
	okLabel := repair.OKLabel
	if strings.Contains(okLabel, "{count}") {
		count := "?"
		if n, err := doctorSessionCount(ctx, stateDBPath); err == nil {
			count = fmt.Sprint(n)
		}
		okLabel = strings.ReplaceAll(okLabel, "{count}", count)
	}
	backupName := "n/a"
	if report.BackupPath != "" {
		backupName = filepath.Base(report.BackupPath)
	}
	doctorCheckOK(d.W, okLabel, "(strategy: "+doctorNoneText(report.Strategy)+"; backup: "+backupName+")")
	f.Fixed++
	return nil
}

// Reports and routes structural damage of the canonical tables, true when that is what the database has
func doctorReportStructuralDamage(ctx context.Context, d *Doctor, f *Finding, shouldFix bool, stateDBPath, reason string) (bool, error) {
	damaged, err := doctorStructuralDamage(stateDBPath)
	if err != nil || !damaged {
		return false, err
	}
	doctorCheckWarn(d.W, d.DisplayHome+"/state.db has structural corruption (canonical tables/indexes damaged, "+
		"not the FTS index)", "("+reason+")")
	return true, doctorRepairStateDB(ctx, d, f, shouldFix, stateDBPath, "structural")
}

// Classifies a state.db that cannot be read, structural damage first and then the malformed schema repair
func doctorClassifyUnreadableStateDB(ctx context.Context, d *Doctor, f *Finding, shouldFix bool, stateDBPath string, cause error) error {
	structural, err := doctorReportStructuralDamage(ctx, d, f, shouldFix, stateDBPath, cause.Error())
	if err != nil || structural {
		return err
	}
	if !state.IsMalformedDBError(cause) {
		doctorCheckWarn(d.W, d.DisplayHome+"/state.db exists but has issues: "+cause.Error(), "")
		return nil
	}
	doctorCheckWarn(d.W, d.DisplayHome+"/state.db schema is malformed (sessions hidden until repaired)", "("+cause.Error()+")")
	return doctorRepairStateDB(ctx, d, f, shouldFix, stateDBPath, "schema")
}

// Reports the session count and the write health probe, classifying a database that cannot be read
func doctorStateDBHealth(ctx context.Context, d *Doctor, f *Finding, shouldFix bool, stateDBPath string) error {
	count, err := doctorSessionCount(ctx, stateDBPath)
	if err != nil {
		return doctorClassifyUnreadableStateDB(ctx, d, f, shouldFix, stateDBPath, err)
	}
	doctorCheckOK(d.W, fmt.Sprintf("%s/state.db exists (%d sessions)", d.DisplayHome, count), "")
	reason, unhealthy, err := doctorWriteHealthReason(ctx, d.W, stateDBPath, shouldFix)
	if err != nil {
		return doctorClassifyUnreadableStateDB(ctx, d, f, shouldFix, stateDBPath, err)
	}
	if !unhealthy {
		return nil
	}
	structural, err := doctorReportStructuralDamage(ctx, d, f, shouldFix, stateDBPath, reason)
	if err != nil || structural {
		return err
	}
	doctorCheckWarn(d.W, d.DisplayHome+"/state.db fails a write-health probe (FTS index may be corrupt)", "("+reason+")")
	return doctorRepairStateDB(ctx, d, f, shouldFix, stateDBPath, "fts")
}

// Prints the read only stats snapshot of state.db, any failure degrading to one info line
func doctorStateDBStats(w io.Writer, issues *[]string, stateDBPath string) error {
	return doctorWarnOnError(w, "state.db stats unavailable ({e})", "", func(w io.Writer, text, _ string) { doctorCheckInfo(w, text) }, func() error {
		stats, err := doctorCollectStateDBStats(stateDBPath)
		if err != nil {
			return err
		}
		var holders *int
		if n, ok := doctorCountDBHolders(stateDBPath); ok {
			holders = &n
		}
		for _, row := range doctorRenderStateDBStats(stats, holders, doctorHostGatewayNote()) {
			if row.Kind != "warn" {
				text := row.Text
				if row.Detail != "" {
					text += " " + row.Detail
				}
				doctorCheckInfo(w, text)
				continue
			}
			doctorCheckWarn(w, row.Text, row.Detail)
			if strings.Contains(row.Detail, "auto_prune") {
				issue := "state.db is large — enable sessions.auto_prune in config.yaml"
				if strings.Contains(row.Detail, "optimize-storage") {
					issue += " and run 'icebath sessions optimize-storage' offline (gateway stopped)"
				}
				*issues = append(*issues, issue)
			}
		}
		return nil
	})
}

// Reports a large WAL file and checkpoints it with fix once the store is proven quiet
func doctorStateDBWAL(ctx context.Context, d *Doctor, f *Finding, shouldFix bool, stateDBPath string) error {
	walPath := filepath.Join(filepath.Dir(stateDBPath), "state.db-wal")
	walSize := func() (int64, error) {
		info, err := os.Stat(walPath)
		if err != nil {
			return 0, nil
		}
		return info.Size(), nil
	}
	return doctorWarnOnError(d.W, "", "", nil, func() error {
		size, err := walSize()
		if err != nil {
			return err
		}
		switch {
		case size > 50*1024*1024:
			title := fmt.Sprintf("WAL file is large (%d MB)", size/(1024*1024))
			skip := "Large WAL file — cannot prove state.db is quiet (stop the profile's gateway first, then " +
				"run 'icebath doctor --fix' to checkpoint)"
			held, err := doctorLiveWriterHoldsDB(ctx, stateDBPath)
			if err != nil {
				return err
			}
			if held {
				doctorCheckWarn(d.W, title, "(normal while Desktop or the gateway is running, or state.db cannot be "+
					"inspected — checkpoint only with them stopped)")
				f.Issues = append(f.Issues, skip)
				return nil
			}
			doctorCheckWarn(d.W, title, "(may indicate missed checkpoints)")
			if !shouldFix {
				f.Issues = append(f.Issues, "Large WAL file — stop the profile's gateway, then run 'icebath doctor --fix' to checkpoint")
				return nil
			}
			skipped := false
			if err := doctorExclusiveRepairDBGuard(ctx, stateDBPath, func(guard state.SQLiteConn, guardErr error) error {
				if guard == nil {
					doctorCheckWarn(d.W, "WAL checkpoint skipped: could not take exclusive ownership of state.db",
						fmt.Sprintf("(%v; stop the profile's gateway and re-run 'icebath doctor --fix')", guardErr))
					f.Issues = append(f.Issues, skip)
					skipped = true
					return nil
				}
				_, err := guard.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
				return err
			}); err != nil || skipped {
				return err
			}
			after, err := walSize()
			if err != nil {
				return err
			}
			doctorCheckOK(d.W, fmt.Sprintf("WAL checkpoint performed (%dK → %dK)", size/1024, after/1024), "")
			f.Fixed++
		case size > 10*1024*1024:
			doctorCheckInfo(d.W, fmt.Sprintf("WAL file is %d MB (normal for active sessions)", size/(1024*1024)))
		}
		return nil
	})
}

// Names the processes holding a retired WAL generation, true when any does
func doctorRetiredWALHolders(d *Doctor, f *Finding, stateDBPath string) bool {
	pids, seen := []int{}, map[int]bool{}
	for _, holder := range doctorDeletedSidecarHolders(stateDBPath) {
		if !seen[holder.PID] {
			seen[holder.PID] = true
			pids = append(pids, holder.PID)
		}
	}
	if len(pids) == 0 {
		return false
	}
	described := make([]string, len(pids))
	for i, pid := range pids {
		described[i] = doctorDescribeHolderPID(pid)
	}
	rendered := strings.Join(described, ", ")
	doctorCheckWarn(d.W, fmt.Sprintf("%s/state.db: %d process(es) still hold a retired WAL generation (%s)", d.DisplayHome, len(pids), rendered),
		"(every new session refuses to open until they exit; health/stats probes skipped)")
	f.Issues = append(f.Issues, "state.db retired WAL generation held by "+rendered+doctorHostGatewayNote()+" — stop the host "+
		"gateway, dashboard and cron writers among them ('icebath "+home.ProfileCLISelector(d.Home)+"gateway "+
		"stop' stops the ONE host process serving every profile, quit the Desktop app), do not "+
		"delete the WAL yourself, then rerun 'icebath doctor'")
	return true
}

// Reports the session count, write health, schema repair, stats snapshot and WAL size of state.db
func checkStateDB(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		stateDBPath := filepath.Join(d.Home, "state.db")
		if doctorRetiredWALHolders(d, f, stateDBPath) {
			return nil
		}
		if _, err := os.Stat(stateDBPath); err == nil {
			if err := doctorStateDBHealth(ctx, d, f, shouldFix, stateDBPath); err != nil {
				return err
			}
			if err := doctorStateDBStats(d.W, &f.Issues, stateDBPath); err != nil {
				return err
			}
		} else {
			doctorCheckInfo(d.W, d.DisplayHome+"/state.db not created yet (will be created on first session)")
		}
		return doctorStateDBWAL(ctx, d, f, shouldFix, stateDBPath)
	})
}

// Reports whether the cron store accepts writes and its filesystem has room left
func checkCronStore(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		cronDir := filepath.Join(d.Home, "cron")
		if report := doctorCronProbeReport(cronDir); report != nil {
			doctorCheckWarn(d.W, d.DisplayHome+"/cron/ is NOT writable ("+pyrepr.Str(report["error"])+") — scheduled cron jobs are being skipped", "")
			f.Issues = append(f.Issues, "Cron store "+pyrepr.Str(report["store"])+" is not writable: "+pyrepr.Str(report["fix"]))
			return nil
		}
		if info, err := os.Stat(cronDir); err == nil && info.IsDir() {
			if free, ok := doctorCronFreeBytes(cronDir); ok && free < cron.LowFreeBytes {
				doctorCheckWarn(d.W, d.DisplayHome+"/cron/ filesystem has only "+doctorHumanBytes(free)+" free — cron jobs stop when it fills", "")
				f.Issues = append(f.Issues, "Free disk space on the filesystem holding "+cronDir)
				return nil
			}
		}
		doctorCheckOK(d.W, d.DisplayHome+"/cron/ store is writable", "")
		return nil
	})
}

// Warns when checkpoints are on and the rollback store sits above its cap
func checkCheckpointStore(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		if notice := doctorCheckpointFootprintNotice(doctorToolsEnv(ctx, d)); notice != "" {
			doctorCheckWarn(d.W, notice, "")
		}
		return nil
	})
}

// Local provenance rows of a plugins dir under the update checker's admission rules
func doctorPluginProvenanceRows(ctx context.Context, pluginsDir string) []doctorStatRow {
	if info, err := os.Stat(pluginsDir); pluginsDir == "" || err != nil || !info.IsDir() {
		return []doctorStatRow{{"info", "No plugins directory yet (nothing to check provenance for)", ""}}
	}
	provenances, err := doctorPluginsProvenance(ctx, pluginsDir)
	if err != nil {
		return []doctorStatRow{{"warn", "Plugin provenance could not be read", err.Error()}}
	}
	rows := []doctorStatRow{}
	for _, provenance := range provenances {
		result := plugins.CheckLocalProvenance(provenance)
		detail := result.NeedsFixing
		if detail == "" {
			detail = result.Reason
		}
		switch {
		case result.NeedsFixing != "" || provenance.Class == plugins.ProvenanceDrift:
			rows = append(rows, doctorStatRow{"warn", "Plugin '" + provenance.Name + "': " + detail, ""})
		case provenance.Class == plugins.ProvenanceManual:
			rows = append(rows, doctorStatRow{"info", "Plugin '" + provenance.Name + "' installed manually", detail})
		case provenance.Class == plugins.ProvenanceSelfCloned:
			rows = append(rows, doctorStatRow{"info", "Plugin '" + provenance.Name + "': self-cloned", detail})
		default:
			rows = append(rows, doctorStatRow{"ok", "Plugin '" + provenance.Name + "': provenance in good standing", detail})
		}
	}
	if len(rows) == 0 {
		return []doctorStatRow{{"info", "No provenanced plugins found", ""}}
	}
	return rows
}

// Prints the local plugin update provenance rows without network or writes
func checkUpdateProvenance(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "", "", func(f *Finding) error {
		for _, row := range doctorPluginProvenanceRows(ctx, filepath.Join(d.Home, "plugins")) {
			switch row.Kind {
			case "warn":
				doctorCheckWarn(d.W, row.Text, row.Detail)
			case "ok":
				doctorCheckOK(d.W, row.Text, row.Detail)
			default:
				doctorCheckInfo(d.W, strings.TrimRight(row.Text+" "+row.Detail, " \t\n\r\x0b\x0c"))
			}
		}
		return nil
	})
}

// Reports whether the gh CLI is logged in, a missing or failing gh reading as not authenticated
func doctorGhAuthenticated(ctx context.Context) bool {
	if safeWhich("gh") == "" {
		return false
	}
	done, err := doctorSubprocessRun(ctx, []string{"gh", "auth", "status"}, pysubprocess.Options{Timeout: 10 * time.Second})
	return err == nil && done.Returncode == 0
}

// Python len of a decoded JSON value, false for a value without a length
func doctorJSONLen(v any) (int, bool) {
	switch x := v.(type) {
	case *pyjson.Object:
		return x.Len(), true
	case map[string]any:
		return len(x), true
	case []any:
		return len(x), true
	case string:
		return len([]rune(x)), true
	}
	return 0, false
}

// Reports the Skills Hub directory, its lock file and quarantine, and the GitHub credential in use
func checkSkillsHub(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		w := d.W
		hubDir := filepath.Join(d.Home, "skills", ".hub")
		_, hubErr := os.Stat(hubDir)
		if doctorCheckBool(w, hubErr == nil, doctorRow{Text: "Skills Hub directory exists"},
			doctorRow{"Skills Hub directory not initialized", "(run: icebath skills list)"}, false) {
			lockFile := filepath.Join(hubDir, "lock.json")
			if _, err := os.Stat(lockFile); err == nil {
				if err := doctorWarnOnError(w, "Lock file", "(corrupted or unreadable)", nil, func() error {
					text, err := doctorReadText(lockFile)
					if err != nil {
						return err
					}
					doc, err := pyjson.Loads(text)
					if err != nil {
						return err
					}
					obj, ok := doc.(*pyjson.Object)
					if !ok {
						return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(doc))
					}
					count := 0
					if installed, present := obj.Get("installed"); present {
						n, sized := doctorJSONLen(installed)
						if !sized {
							return fmt.Errorf("object of type '%s' has no len()", pyrepr.TypeName(installed))
						}
						count = n
					}
					doctorCheckOK(w, fmt.Sprintf("Lock file OK (%d hub-installed skill(s))", count), "")
					return nil
				}); err != nil {
					return err
				}
			}
			quarantine := filepath.Join(hubDir, "quarantine")
			quarantined := 0
			if _, err := os.Stat(quarantine); err == nil {
				entries, err := os.ReadDir(quarantine)
				if err != nil {
					return err
				}
				for _, entry := range entries {
					if info, err := os.Stat(filepath.Join(quarantine, entry.Name())); err == nil && info.IsDir() {
						quarantined++
					}
				}
			}
			if quarantined > 0 {
				doctorCheckWarn(w, fmt.Sprintf("%d skill(s) in quarantine", quarantined), "(pending review)")
			}
		}
		for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
			value, _, err := config.GetEnvValue(ctx, d.Home, key)
			if err != nil {
				return err
			}
			if value != "" {
				doctorCheckOK(w, "GitHub token configured", "(validity checked under API Connectivity)")
				return nil
			}
		}
		doctorCheckBool(w, doctorGhAuthenticated(ctx), doctorRow{"GitHub authenticated via gh CLI", "(full API access — no GITHUB_TOKEN needed)"},
			doctorRow{"No GITHUB_TOKEN", "(60 req/hr rate limit — set in " + d.DisplayHome + "/.env for better rates)"}, false)
		return nil
	})
}

// Reports one memory provider as active, configured but unavailable, or not found with the install command
func doctorMemoryProviderGeneric(ctx context.Context, d *Doctor, name string) {
	env := doctorToolsEnv(ctx, d)
	provider := doctorLoadMemoryProvider(ctx, env, name)
	switch {
	case provider != nil && provider.Available(ctx):
		doctorCheckOK(d.W, name+" provider active", "")
	case provider != nil:
		doctorCheckWarn(d.W, name+" configured but not available", "run: icebath memory status")
	default:
		hint := ""
		if memory.FindProviderDir(ctx, env, name) == "" {
			hint = memory.CatalogInstallHint(ctx, name, "memory")
		}
		if hint == "" {
			hint = "icebath memory setup"
		}
		doctorCheckWarn(d.W, name+" plugin not found", "run: "+hint)
	}
}

// Reports the built in memory as active or the state of the configured external memory provider
func checkMemoryProvider(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		name := ""
		if v, ok := doctorMemoryConfig(ctx, d.Home)["provider"]; ok && v != nil {
			name = pyrepr.Str(v)
		}
		if memory.IsCoreMemoryProvider(name) {
			doctorCheckOK(d.W, "Built-in memory active", "(no external provider configured — this is fine)")
			return nil
		}
		if _, parked := memory.ParkedProviderNotice(ctx, doctorToolsEnv(ctx, d), name); parked {
			doctorCheckWarn(d.W, name+" is turned off", "(plugins.disabled lists it, so built-in memory is used)")
			f.ManualIssues = append(f.ManualIssues, "Memory provider '"+name+"' is turned off — run: icebath plugins enable "+name)
			return nil
		}
		failure := func() (failure error) {
			defer func() {
				if r := recover(); r != nil {
					failure = fmt.Errorf("%v", r)
				}
			}()
			if checker, ok := doctorMemoryProviderChecks[name]; ok {
				return checker(ctx, d, &f.Issues)
			}
			doctorMemoryProviderGeneric(ctx, d, name)
			return nil
		}()
		if failure != nil {
			doctorCheckWarn(d.W, name+" check failed", failure.Error())
		}
		return nil
	})
}

// Key order of the mapping at a path of a YAML file, nil when no mapping sits there
func doctorYAMLKeyOrder(path string, keys ...string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	node := doc.Content[0]
	for _, key := range keys {
		if node.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				next = node.Content[i+1]
			}
		}
		if next == nil {
			return nil
		}
		node = next
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	order := []string{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		order = append(order, node.Content[i].Value)
	}
	return order
}

// Reports each plugin an update turned off and still lists as disabled
func checkPluginsDisabledByUpdate(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "", "", func(f *Finding) error {
		selection, err := pm.ReadHomeSelection(d.Home)
		if err != nil {
			return err
		}
		pluginsSection, _ := selection["plugins"].(map[string]any)
		record, isRecord := pluginsSection[pm.DisabledByUpdateRecord].(map[string]any)
		if !isRecord {
			return nil
		}
		disabled := map[string]bool{}
		switch listed := pluginsSection["disabled"].(type) {
		case []any:
			for _, name := range listed {
				disabled[pyrepr.Str(name)] = true
			}
		case string:
			for _, r := range listed {
				disabled[string(r)] = true
			}
		}
		order := doctorYAMLKeyOrder(filepath.Join(d.Home, "config.yaml"), "plugins", pm.DisabledByUpdateRecord)
		ordered, seen := []string{}, map[string]bool{}
		for _, name := range order {
			if _, ok := record[name]; ok && !seen[name] {
				seen[name] = true
				ordered = append(ordered, name)
			}
		}
		rest := []string{}
		for name := range record {
			if !seen[name] {
				rest = append(rest, name)
			}
		}
		sort.Strings(rest)
		names := []string{}
		for _, name := range append(ordered, rest...) {
			if disabled[name] {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			return nil
		}
		doctorSection(d.W, "Plugins Turned Off by an Update")
		for _, name := range names {
			reason := ""
			if entry, ok := record[name].(map[string]any); ok {
				if v, present := entry["reason"]; present && doctorTruthy(v) {
					reason = pyrepr.Str(v)
				}
			}
			detail := ""
			if reason != "" {
				detail = "(" + reason + ")"
			}
			doctorCheckWarn(d.W, name+" is off", detail)
			f.ManualIssues = append(f.ManualIssues, "Plugin '"+name+"' was turned off by an update; a later update retries it, "+
				"or run: icebath plugins enable "+name)
		}
		return nil
	})
}

// Reports each plugin both plugins.enabled and plugins.disabled name, which the loader leaves off
func checkPluginsInBothLists(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "", "", func(f *Finding) error {
		both := doctorPluginsInBothLists(d.Home)
		if len(both) == 0 {
			return nil
		}
		doctorSection(d.W, "Plugins Listed as Both Enabled and Disabled")
		for _, name := range both {
			doctorCheckWarn(d.W, name+" will not load", "(plugins.disabled wins)")
			f.ManualIssues = append(f.ManualIssues, "Plugin '"+name+"' is in both plugins.enabled and plugins.disabled, so it is off. "+
				"Turn it on with `icebath plugins enable "+name+"` (a memory provider also needs "+
				"memory.provider set), or keep it off with `icebath plugins disable "+name+"`")
		}
		return nil
	})
}

// Reports each named profile, orphan alias wrappers and platform credentials duplicated across profiles
func checkProfiles(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "", "", func(f *Finding) error {
		all, err := doctorListProfiles(ctx)
		if err != nil {
			return err
		}
		named := []manage.ProfileInfo{}
		for _, p := range all {
			if !p.IsDefault {
				named = append(named, p)
			}
		}
		if len(named) == 0 {
			return nil
		}
		w := d.W
		doctorSection(w, "Profiles")
		doctorCheckOK(w, fmt.Sprintf("%d profile(s) found", len(named)), "")
		wrapperDir, err := profiles.WrapperDir()
		if err != nil {
			return err
		}
		for _, p := range named {
			parts := []string{}
			if p.GatewayRunning {
				parts = append(parts, "gateway running")
			}
			if config.PyTruthy(p.Model) {
				model, ok := p.Model.(string)
				if !ok {
					return fmt.Errorf("'%s' object is not subscriptable", pyrepr.TypeName(p.Model))
				}
				if runes := []rune(model); len(runes) > 30 {
					model = string(runes[:30])
				}
				parts = append(parts, model)
			}
			exists, err := config.ConfigExists(ctx, filepath.Join(p.Path, "config.yaml"))
			if err != nil {
				return err
			}
			if !exists {
				parts = append(parts, "⚠ missing config")
			}
			if _, err := os.Stat(filepath.Join(p.Path, ".env")); err != nil {
				parts = append(parts, "no .env")
			}
			if p.AliasPath == "" {
				parts = append(parts, "no alias")
			}
			status := "configured"
			if len(parts) > 0 {
				status = strings.Join(parts, ", ")
			}
			doctorCheckOK(w, "  "+p.Name+": "+status, "")
		}
		if info, err := os.Stat(wrapperDir); err == nil && info.IsDir() {
			entries, err := os.ReadDir(wrapperDir)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				wrapper := filepath.Join(wrapperDir, entry.Name())
				if info, err := os.Stat(wrapper); err != nil || !info.Mode().IsRegular() {
					continue
				}
				if err := doctorWarnOnError(w, "", "", nil, func() error {
					text, err := doctorReadText(wrapper)
					if err != nil {
						return err
					}
					match := doctorOrphanWrapperPattern.FindStringSubmatch(text)
					if match == nil || profiles.ProfileExists(match[1]) {
						return nil
					}
					stem := pypath.PathStem(entry.Name())
					alias := entry.Name()
					if owned, err := profiles.WrapperPath(stem); err != nil {
						return err
					} else if owned == wrapper {
						alias = stem
					}
					fix := "delete " + wrapper
					if home.ProfileIDRe.MatchString(alias) {
						fix = "icebath profile alias " + alias + " --remove"
					}
					doctorCheckWarn(w, "Orphan alias: "+alias+" → profile '"+match[1]+"' no longer exists", "(remove with: "+fix+")")
					return nil
				}); err != nil {
					return err
				}
			}
		}
		findings, err := doctorDuplicateCredentialFindings(ctx)
		if err != nil {
			return err
		}
		for _, line := range findings {
			doctorCheckWarn(w, "Duplicate platform credential across profiles", "("+line+")")
			f.ManualIssues = append(f.ManualIssues, line)
		}
		return nil
	})
}
