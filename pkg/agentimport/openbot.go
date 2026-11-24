package agentimport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	_ "modernc.org/sqlite"
)

const (
	openbotAgent    = "openbot"
	openbotDatabase = "openbot.db"
	weekdays        = "1-5"
)

// OpenBot user data folder per OS
var openbotUserDataDirs = []string{"Library/Application Support/OpenBot", ".config/OpenBot", "AppData/Roaming/OpenBot"}

// Workspace skill folders OpenBot lists
var openbotSkillDirs = []string{".agents/skills", ".claude/skills", ".opencode/skills", ".gemini/skills", ".cursor/skills",
	".cline/skills", ".clinerules/skills"}

// A cron field N/S with no range fires at N only in OpenBot
var bareStepRe = pyre.MustCompile(`^\d+/\d+$`)

// FindOpenBotUserData is the folder holding openbot.db, from the source folder or file else the OS default
func FindOpenBotUserData(source string) string {
	var candidates []string
	if source != "" {
		resolved, err := pypath.Resolve(pypath.Expanduser(source))
		if err != nil {
			resolved = pypath.Expanduser(source)
		}
		candidates = []string{resolved}
	} else {
		for _, d := range openbotUserDataDirs {
			candidates = append(candidates, filepath.Join(userHome(), d))
		}
	}
	for _, c := range candidates {
		if isFile(c) && filepath.Base(c) == openbotDatabase {
			return filepath.Dir(c)
		}
		if isFile(filepath.Join(c, openbotDatabase)) {
			return c
		}
	}
	return ""
}

// Python int of a decoded value, false where int raises
func pyInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, false
		}
		return int64(t), true
	case *big.Int:
		return t.Int64(), t.IsInt64()
	case string:
		n, ok := pystr.IntText(t)
		if !ok || !n.IsInt64() {
			return 0, false
		}
		return n.Int64(), true
	}
	return 0, false
}

// Hour and minute of a clock time as the minute and hour cron fields
func clockFields(v any) (string, bool) {
	parts := strings.Split(pyStr(v), ":")
	if len(parts) != 2 {
		return "", false
	}
	hour, ok1 := pyInt(parts[0])
	minute, ok2 := pyInt(parts[1])
	if !ok1 || !ok2 {
		return "", false
	}
	return fmt.Sprintf("%d %d", minute, hour), true
}

// Required key of a schedule object, false when missing
func need(s *pyjson.Object, key string) (any, bool) {
	v, ok := s.Vals[key]
	return v, ok
}

// UTC offset zoneinfo gives a wall time with fold 0, the earlier offset in a gap or an overlap
func wallOffset(loc *time.Location, wall time.Time) int {
	w := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, time.UTC).Unix()
	p := w - 2*86400
	for range 64 {
		at := time.Unix(p, 0).In(loc)
		_, off := at.Zone()
		_, end := at.ZoneBounds()
		if end.IsZero() {
			return off
		}
		_, after := end.In(loc).Zone()
		if w < end.Unix()+int64(max(off, after)) {
			return off
		}
		p = end.Unix()
	}
	_, off := time.Unix(w, 0).In(loc).Zone()
	return off
}

// ScheduleToCron maps an OpenBot routine schedule to a cron schedule firing at the same instants, false when none fits
func ScheduleToCron(schedule *pyjson.Object, timezone string) (string, bool) {
	if timezone == "" {
		timezone = "UTC"
	}
	zone, err := pydatetime.ZoneInfo(timezone)
	if err != nil {
		zone, _ = pydatetime.ZoneInfo("UTC")
	}
	kind, _ := schedule.Vals["kind"].(string)
	switch kind {
	case "hourly":
		m, ok := need(schedule, "minute")
		if !ok {
			return "", false
		}
		minute, ok := pyInt(m)
		if !ok {
			return "", false
		}
		return fmt.Sprintf("%d * * * *", minute), true
	case "daily", "weekdays", "weekly", "monthly":
		t, ok := need(schedule, "time")
		if !ok {
			return "", false
		}
		clock, ok := clockFields(t)
		if !ok {
			return "", false
		}
		switch kind {
		case "daily":
			return clock + " * * *", true
		case "weekdays":
			return clock + " * * " + weekdays, true
		case "weekly":
			d, ok := need(schedule, "weekday")
			n, ok2 := pyInt(d)
			if !ok || !ok2 {
				return "", false
			}
			return fmt.Sprintf("%s * * %d", clock, n), true
		default:
			d, ok := need(schedule, "day")
			n, ok2 := pyInt(d)
			if !ok || !ok2 {
				return "", false
			}
			return fmt.Sprintf("%s %d * *", clock, n), true
		}
	case "interval":
		return intervalCron(schedule)
	case "custom":
		return customCron(schedule)
	case "once":
		d, ok1 := need(schedule, "date")
		t, ok2 := need(schedule, "time")
		if !ok1 || !ok2 {
			return "", false
		}
		parsed, _, err := pydatetime.ParseISOFormat(pyStr(d) + "T" + pyStr(t))
		if err != nil {
			return "", false
		}
		y, mo, dd := parsed.Date()
		wall := time.Date(y, mo, dd, parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(), time.UTC)
		off := wallOffset(zone, wall)
		return pydatetime.ISOFormat(time.Date(y, mo, dd, parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(), time.FixedZone("", off))), true
	case "advanced":
		return advancedCron(schedule)
	}
	return "", false
}

// Interval kept on its anchor phase when the step divides the hour or day, else an interval from the import
func intervalCron(schedule *pyjson.Object) (string, bool) {
	a, ok1 := need(schedule, "amount")
	u, ok2 := need(schedule, "unit")
	anchorRaw, ok3 := need(schedule, "anchorAt")
	if !ok1 || !ok2 || !ok3 {
		return "", false
	}
	amount, ok := pyInt(a)
	if !ok || amount == 0 {
		return "", false
	}
	unit := pyStr(u)
	parsed, aware, err := pydatetime.ParseISOFormat(strings.ReplaceAll(pyStr(anchorRaw), "Z", "+00:00"))
	if err != nil {
		return "", false
	}
	anchor := parsed.UTC()
	if !aware {
		secs, ok := pydatetime.LocalToSeconds(parsed, time.Local, 0, pydatetime.LocaltimeAccepts)
		if !ok {
			return "", false
		}
		anchor = time.Unix(secs, int64(parsed.Nanosecond())).UTC()
	}
	switch {
	case unit == "minutes" && pyMod(60, amount) == 0:
		if amount > 1 {
			return fmt.Sprintf("%d-59/%d * * * *", pyMod(int64(anchor.Minute()), amount), amount), true
		}
		return "* * * * *", true
	case unit == "hours" && pyMod(24, amount) == 0:
		return fmt.Sprintf("%d %d-23/%d * * *", anchor.Minute(), pyMod(int64(anchor.Hour()), amount), amount), true
	case unit == "days" && amount == 1:
		return fmt.Sprintf("%d %d * * *", anchor.Minute(), anchor.Hour()), true
	}
	if unit == "" {
		return "", false
	}
	return fmt.Sprintf("every %d%s", amount, string([]rune(unit)[0])), true
}

// Remainder with the sign of the divisor like the Python modulo operator
func pyMod(a, b int64) int64 {
	r := a % b
	if r != 0 && (r < 0) != (b < 0) {
		r += b
	}
	return r
}

// Custom expression as given unless a bare step field would fire more often than in OpenBot
func customCron(schedule *pyjson.Object) (string, bool) {
	e, ok := need(schedule, "expression")
	if !ok {
		return "", false
	}
	expression := strings.TrimSpace(pyStr(e))
	for _, f := range pystr.SplitWhitespace(expression, -1) {
		if reMatch(bareStepRe, f) {
			return "", false
		}
	}
	return expression, true
}

// Comma list of the integers a value iterates over, false when one is not an integer
func intList(v any) (string, bool) {
	var list []any
	switch t := v.(type) {
	case []any:
		list = t
	case string:
		for _, r := range t {
			list = append(list, string(r))
		}
	case *pyjson.Object:
		for _, k := range t.Keys {
			list = append(list, k)
		}
	default:
		return "", false
	}
	parts := make([]string, len(list))
	for i, item := range list {
		n, ok := pyInt(item)
		if !ok {
			return "", false
		}
		parts[i] = strconv.FormatInt(n, 10)
	}
	return strings.Join(parts, ","), true
}

// Months, day rule and time rule of an advanced schedule as one cron expression
func advancedCron(schedule *pyjson.Object) (string, bool) {
	m, ok := need(schedule, "months")
	if !ok {
		return "", false
	}
	months, ok := intList(m)
	if !ok {
		return "", false
	}
	if months == "1,2,3,4,5,6,7,8,9,10,11,12" {
		months = "*"
	}
	days, ok1 := asObject(schedule.Vals["days"])
	tm, ok2 := asObject(schedule.Vals["time"])
	if !ok1 || !ok2 {
		return "", false
	}
	listed := ""
	if raw, has := days.Vals["days"]; has {
		var ok bool
		if listed, ok = intList(raw); !ok {
			return "", false
		}
	}
	var dom, dow string
	switch pyStr(days.Vals["kind"]) {
	case "every-day":
		dom, dow = "*", "*"
	case "days-of-week":
		dom, dow = "*", listed
	case "days-of-month":
		dom, dow = listed, "*"
	default:
		return "", false
	}
	var clock string
	tkind, hasKind := tm.Vals["kind"]
	if !hasKind {
		return "", false
	}
	if pyStr(tkind) == "at-time" {
		t, has := tm.Vals["time"]
		if !has {
			return "", false
		}
		c, ok := clockFields(t)
		if !ok {
			return "", false
		}
		clock = c
	} else {
		unit, has := tm.Vals["unit"]
		amountRaw, hasAmount := tm.Vals["amount"]
		if !has || !hasAmount {
			return "", false
		}
		amount, ok := pyInt(amountRaw)
		if !ok {
			return "", false
		}
		if pyStr(unit) == "minutes" {
			clock = fmt.Sprintf("*/%d *", amount)
		} else {
			clock = fmt.Sprintf("0 */%d", amount)
		}
	}
	return fmt.Sprintf("%s %s %s %s", clock, dom, months, dow), true
}

// SQLite result code of a driver error, false for an error that is not one
func sqliteCode(err error) (int, bool) {
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		return coded.Code(), true
	}
	return 0, false
}

// Trailing result code the driver appends to an sqlite message
var sqliteCodeSuffix = pyre.MustCompile(`^(.*) \(\d+\)( \(SQLITE_BUSY\))?$`)

// Python str of an error, an sqlite driver error reduced to the sqlite errmsg sqlite3 shows
func sqliteErrText(err error) string {
	var coded interface {
		Code() int
		Error() string
	}
	if !errors.As(err, &coded) {
		return pyErrText(err)
	}
	msg := coded.Error()
	if m, _ := sqliteCodeSuffix.FindStringMatch(msg); m != nil {
		msg = m.GroupByNumber(1).String()
	}
	if _, detail, found := strings.Cut(msg, ": "); found {
		return detail
	}
	return msg
}

// Reports whether a driver error is the OperationalError of a table or column this OpenBot lacks
func isOperationalError(err error) bool {
	code, ok := sqliteCode(err)
	if !ok {
		return false
	}
	return (&pysqlite.Error{Code: code}).PyTypeName() == "OperationalError"
}

// Private in memory copy of the live database, a read only open first and a copy of its files second
func openSnapshot(ctx context.Context, database, scratch string) (*sql.DB, *sql.Conn, error) {
	snapDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, err
	}
	snapDB.SetMaxOpenConns(1)
	snap, err := snapDB.Conn(ctx)
	if err != nil {
		snapDB.Close()
		return nil, nil, err
	}
	fail := func(err error) (*sql.DB, *sql.Conn, error) {
		snap.Close()
		snapDB.Close()
		return nil, nil, err
	}
	backupFrom := func(dsn string) error {
		src, err := sql.Open("sqlite", dsn)
		if err != nil {
			return err
		}
		defer src.Close()
		conn, err := src.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		return pysqlite.Backup(conn, snap, -1, nil, 0)
	}
	escape := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")
	if err := backupFrom("file:" + escape.Replace(filepath.ToSlash(database)) + "?mode=ro"); err != nil {
		var pe *pysqlite.Error
		if _, isSQLite := sqliteCode(err); !isSQLite && !errors.As(err, &pe) {
			return fail(err)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			part := database + suffix
			if isFile(part) {
				if err := pyshutil.Copy2(part, filepath.Join(scratch, filepath.Base(part))); err != nil {
					return fail(err)
				}
			}
		}
		copyPath := filepath.Join(scratch, filepath.Base(database))
		if err := backupFrom("file:" + escape.Replace(filepath.ToSlash(copyPath))); err != nil {
			return fail(err)
		}
	}
	return snapDB, snap, nil
}

// Rows of a query as column maps, a table or column this OpenBot lacks giving none
func queryRows(ctx context.Context, db *sql.Conn, query string, args ...any) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		if isOperationalError(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				row[c] = string(b)
			} else {
				row[c] = vals[i]
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		if isOperationalError(err) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// Decoded JSON of a column, the default when empty or not JSON
func columnJSON(raw any, def any) any {
	s, ok := raw.(string)
	if !ok || s == "" {
		return def
	}
	v, err := pyjson.Loads(s)
	if err != nil {
		return def
	}
	return v
}

// Python truthiness of a column value
func colTruthy(v any) bool { return truthy(v) }

// Python str of a column value
func colStr(v any) string { return pyStr(v) }

// The agent's own skill folders, one per name, bundled skills left behind
func openbotSkills(workspace string) ([]any, error) {
	found := pyjson.NewObject()
	for _, rel := range openbotSkillDirs {
		root := filepath.Join(workspace, filepath.FromSlash(rel))
		if !isDir(root) {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		var folders []string
		for _, e := range entries {
			folders = append(folders, filepath.Join(root, e.Name()))
		}
		sortPaths(folders)
		for _, folder := range folders {
			if isFile(filepath.Join(folder, "SKILL.md")) && !exists(filepath.Join(folder, ".openbot-managed.json")) {
				name := filepath.Base(folder)
				if _, has := found.Vals[name]; !has {
					found.Set(name, pyjson.ObjectOf("name", name, "path", folder))
				}
			}
		}
	}
	out := make([]any, 0, found.Len())
	for _, k := range found.Keys {
		out = append(out, found.Vals[k])
	}
	return out, nil
}

// Key and value pairs OpenBot stores for env vars and headers as one object
func openbotPairs(raw any) *pyjson.Object {
	out := pyjson.NewObject()
	items, ok := columnJSON(raw, []any{}).([]any)
	if !ok {
		return out
	}
	for _, item := range items {
		o, ok := asObject(item)
		if !ok || !truthy(o.Vals["key"]) {
			continue
		}
		value := ""
		if v, has := o.Vals["value"]; has {
			value = pyStr(v)
		}
		out.Set(pyStr(o.Vals["key"]), value)
	}
	return out
}

// OpenBotData holds the agents as normalized bots, the machine wide MCP servers, their env passthrough names and unreadable rows
type OpenBotData struct {
	Agents         []*pyjson.Object
	MCPServers     *pyjson.Object
	PassthroughFor []string
	Passthrough    map[string][]string
	Problems       []string
}

func readMCPServers(ctx context.Context, db *sql.Conn, data *OpenBotData) error {
	rows, err := queryRows(ctx, db, "SELECT * FROM projection_mcp_servers WHERE enabled = 1 ORDER BY position")
	if err != nil {
		return err
	}
	for _, row := range rows {
		name := ""
		if colTruthy(row["name"]) {
			name = strings.TrimSpace(colStr(row["name"]))
		}
		if name == "" {
			continue
		}
		entry := pyjson.NewObject()
		if s, _ := row["transport"].(string); s == "http" {
			url := any("")
			if colTruthy(row["url"]) {
				url = row["url"]
			}
			entry.Set("url", url)
			entry.Set("headers", openbotPairs(row["headers_json"]))
		} else {
			command := any("")
			if colTruthy(row["command"]) {
				command = row["command"]
			}
			entry.Set("command", command)
			entry.Set("args", columnJSON(row["args_json"], []any{}))
			entry.Set("env", openbotPairs(row["env_json"]))
			if colTruthy(row["working_directory"]) {
				entry.Set("cwd", row["working_directory"])
			}
		}
		if !truthy(entry.Vals["url"]) && !truthy(entry.Vals["command"]) {
			data.Problems = append(data.Problems, fmt.Sprintf("MCP server %s has no command or URL", name))
			continue
		}
		data.MCPServers.Set(name, entry)
		var names []string
		switch v := columnJSON(row["env_passthrough_json"], []any{}).(type) {
		case []any:
			for _, n := range v {
				if truthy(n) {
					names = append(names, pyStr(n))
				}
			}
		case *pyjson.Object:
			for _, n := range v.Keys {
				if n != "" {
					names = append(names, n)
				}
			}
		case string:
			for _, r := range v {
				names = append(names, string(r))
			}
		}
		if len(names) > 0 {
			if _, had := data.Passthrough[name]; !had {
				data.PassthroughFor = append(data.PassthroughFor, name)
			}
			data.Passthrough[name] = names
		}
	}
	return nil
}

// One OpenBot routine row as a normalized routine
func openbotRoutine(r map[string]any) *pyjson.Object {
	schedule := columnJSON(r["schedule_json"], nil)
	timezone := "UTC"
	if colTruthy(r["timezone"]) {
		timezone = colStr(r["timezone"])
	}
	cronExpr, hasCron := "", false
	sched, isObj := asObject(schedule)
	if isObj {
		cronExpr, hasCron = ScheduleToCron(sched, timezone)
	}
	kind, _ := get(schedule, "kind").(string)
	interval := isObj && kind == "interval"
	name := any("routine")
	if colTruthy(r["name"]) {
		name = r["name"]
	}
	instruction := any("")
	if colTruthy(r["instruction"]) {
		instruction = r["instruction"]
	}
	zone := timezone
	if interval {
		zone = "UTC"
	}
	active, has := r["active"]
	if !has {
		active = int64(1)
	}
	trigger := "other"
	if hasCron {
		trigger = "schedule"
	}
	details := ""
	if !hasCron {
		if truthy(schedule) {
			text, _ := pyjson.Dumps(schedule)
			details = "OpenBot schedule " + text
		} else {
			details = "No schedule: run by hand, a script or a webhook in OpenBot"
		}
	}
	return pyjson.ObjectOf("name", name, "instruction", instruction, "timezone", zone, "paused", !truthy(active),
		"trigger", trigger, "schedule_cron", cronExpr, "trigger_details", details)
}

// ReadOpenBotAgents reads every agent and the MCP servers, one unreadable agent reported and a foreign database refused
func ReadOpenBotAgents(ctx context.Context, userData, scratchParent string) (*OpenBotData, error) {
	if err := os.MkdirAll(scratchParent, 0o777); err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp(scratchParent, "openbot-import-")
	if err != nil {
		return nil, err
	}
	snapDB, db, err := openSnapshot(ctx, filepath.Join(userData, openbotDatabase), scratch)
	if rmErr := os.RemoveAll(scratch); err == nil && rmErr != nil {
		if db != nil {
			db.Close()
			snapDB.Close()
		}
		return nil, rmErr
	}
	if err != nil {
		return nil, err
	}
	defer snapDB.Close()
	defer db.Close()
	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'projection_agents'").Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("no agents table: not an OpenBot database, or a version this importer does not know (update Icebath: icebath update)")
		}
		return nil, err
	}
	data := &OpenBotData{MCPServers: pyjson.NewObject(), Passthrough: map[string][]string{}}
	agentRows, err := queryRows(ctx, db, "SELECT * FROM projection_agents ORDER BY sort_order")
	if err != nil {
		return nil, err
	}
	for _, row := range agentRows {
		agentID := ""
		if colTruthy(row["agent_id"]) {
			agentID = colStr(row["agent_id"])
		}
		agent, ok := asObject(columnJSON(row["agent_json"], nil))
		if !ok {
			shown := agentID
			if shown == "" {
				shown = "?"
			}
			data.Problems = append(data.Problems, fmt.Sprintf("agent %s: unreadable settings, skipped", shown))
			continue
		}
		workspace := filepath.Join(userHome(), "OpenBot", "Agents", agentID)
		if truthy(agent.Vals["workspacePath"]) {
			workspace = pyStr(agent.Vals["workspacePath"])
		}
		routineRows, err := queryRows(ctx, db, "SELECT r.*, t.schedule_json FROM projection_agent_routines r LEFT JOIN "+
			"projection_routine_triggers t ON t.routine_id = r.routine_id WHERE r.agent_id = ? "+
			"ORDER BY r.created_at", agentID)
		if err != nil {
			return nil, err
		}
		routines := make([]any, 0, len(routineRows))
		for _, r := range routineRows {
			routines = append(routines, openbotRoutine(r))
		}
		memoryRows, err := queryRows(ctx, db, "SELECT text FROM projection_agent_memories WHERE agent_id = ? ORDER BY created_at", agentID)
		if err != nil {
			return nil, err
		}
		memories := []any{}
		for _, m := range memoryRows {
			if colTruthy(m["text"]) {
				memories = append(memories, m["text"])
			}
		}
		skills, err := openbotSkills(workspace)
		if err != nil {
			return nil, err
		}
		name, title, description := any(agentID), any(""), any("")
		if truthy(agent.Vals["name"]) {
			name = agent.Vals["name"]
		}
		if truthy(agent.Vals["title"]) {
			title = agent.Vals["title"]
		}
		if truthy(agent.Vals["description"]) {
			description = agent.Vals["description"]
		}
		data.Agents = append(data.Agents, pyjson.ObjectOf("name", name, "title", title, "instructions", description,
			"memories", memories, "skills", skills, "routines", routines))
	}
	if err := readMCPServers(ctx, db, data); err != nil {
		return nil, err
	}
	return data, nil
}

// NewOpenBotImporter builds the importer of the OpenBot agents under a user data folder
func NewOpenBotImporter(ctx context.Context, userData, targetRoot string, o BotOptions) *BotImporter {
	database := filepath.Join(userData, openbotDatabase)
	g := newBotImporter(ctx, openbotAgent, "OpenBot", "openbot-imports", []string{database}, targetRoot, o, nil)
	data := &OpenBotData{MCPServers: pyjson.NewObject(), Passthrough: map[string][]string{}}
	g.loadBots = func() []sourcedBot {
		read, err := ReadOpenBotAgents(ctx, userData, filepath.Join(targetRoot, "cache"))
		if err != nil {
			g.record("database", database, "", "error", "Could not read OpenBot's database: "+sqliteErrText(err), nil)
			return nil
		}
		*data = *read
		for _, problem := range data.Problems {
			g.record("database", database, "", "skipped", problem, nil)
		}
		if len(data.Agents) == 0 {
			g.record("agents", database, "", "skipped", "No agents in OpenBot", nil)
		}
		out := make([]sourcedBot, 0, len(data.Agents))
		for _, a := range data.Agents {
			out = append(out, sourcedBot{path: database, bot: a})
		}
		return out
	}
	g.importShared = func() error {
		if data.MCPServers.Len() > 0 {
			if err := g.importMCPServers(data.MCPServers, "mcp-servers"); err != nil {
				return err
			}
		}
		for _, name := range data.PassthroughFor {
			g.record("mcp-env-passthrough", name, "", "skipped",
				fmt.Sprintf("OpenBot passed %s to this server from its environment; add them under mcp_servers.%s.env (secrets in the profile's .env)",
					strings.Join(data.Passthrough[name], ", "), name), nil)
		}
		return nil
	}
	return g
}
