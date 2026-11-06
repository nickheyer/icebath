package updaterecord

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

const (
	UpdateMarkerMaxAgeSeconds  = 20 * 60
	CreateTimeToleranceSeconds = 2.0
	ownCreateTimeEpsilon       = 0.005
	wholeSecondCT              = 1.0
	EmptyMarkerGraceSeconds    = 5.0
	MarkerName                 = ".icebath-update-in-progress"
	CheckoutLockName           = ".icebath-update.lock"
	GitCheckoutLockName        = "icebath-update.lock"
	HandoffPIDEnv              = "ICEBATH_UPDATE_HANDOFF_PID"
	UpdateExitConcurrent       = 2
	u32Max                     = 0xFFFFFFFF
	u64Max                     = 0xFFFFFFFFFFFFFFFF
)

var (
	intLineRE      = regexp.MustCompile(`^[0-9]+$`)
	ctValueRE      = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)
	ctLineRE       = regexp.MustCompile(`^ct:([0-9]+(?:\.[0-9]+)?)$`)
	delegateLineRE = regexp.MustCompile(`^delegate:([0-9]+) ct:([0-9]+(?:\.[0-9]+)?)$`)
	runLineRE      = regexp.MustCompile(`^run:([A-Za-z0-9._-]{1,128})$`)
)

// Shared update marker in the profile tree root home every update owner watches
func UpdateMarkerPath() string {
	return filepath.Join(home.Root(), MarkerName)
}

// Checkout lock of an install, inside its common git dir for a checkout, the running install when empty
func CheckoutLockPath(installRoot string) (string, error) {
	if installRoot == "" {
		installRoot = pm.RepoRoot()
	}
	common, err := GitCommonDir(installRoot)
	if err != nil {
		return "", err
	}
	if common == "" {
		return filepath.Join(installRoot, CheckoutLockName), nil
	}
	return filepath.Join(common, GitCheckoutLockName), nil
}

// Zombie aware liveness of a positive pid
func pidAlive(pid int) bool {
	return pid > 0 && proc.Exists(pid)
}

// Creation time of pid in unix seconds on the clock psutil reports, nil when unreadable
func ProcessCreateTime(pid int) *float64 {
	birth, err := proc.Birth(pid)
	if err != nil {
		return nil
	}
	return &birth
}

var ownCT struct {
	sync.Mutex
	pid int
	ct  *float64
}

// This process's creation time, cached per pid
func ownCreateTime() *float64 {
	ownCT.Lock()
	defer ownCT.Unlock()
	if pid := os.Getpid(); ownCT.pid != pid {
		ownCT.pid, ownCT.ct = pid, ProcessCreateTime(pid)
	}
	return ownCT.ct
}

// Recorded creation time as a float from a number or ct text, nil for anything else
func asCT(recorded any) *float64 {
	var value float64
	switch v := recorded.(type) {
	case nil:
		return nil
	case bool:
		if v {
			value = 1
		}
	case int:
		value = float64(v)
	case int64:
		value = float64(v)
	case float64:
		value = v
	case string:
		text := strings.TrimPrefix(pystr.Strip(v), "ct:")
		if !ctValueRE.MatchString(text) {
			return nil
		}
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil
		}
		value = parsed
	default:
		return nil
	}
	return &value
}

type World struct {
	PID   int
	CT    *float64
	Now   float64
	Alive func(int) bool
	CTOf  func(int) *float64
}

var (
	aliveOf      = pidAlive
	createTimeOf = ProcessCreateTime
	markerReader = readMarker
)

// The running process, the wall clock and the live process table
func realWorld() World {
	return World{PID: os.Getpid(), CT: ownCreateTime(), Now: float64(time.Now().UnixNano()) / 1e9, Alive: aliveOf, CTOf: createTimeOf}
}

func boolPtr(value bool) *bool { return &value }

// One incarnation rule, true running, false dead or another incarnation, nil alive but unprovable
func incarnation(pid int, recorded *float64, w World) *bool {
	if pid <= 0 {
		return boolPtr(false)
	}
	if pid == w.PID {
		switch {
		case w.CT == nil:
			return boolPtr(recorded == nil)
		case recorded == nil:
			return boolPtr(false)
		case math.Abs(*w.CT-*recorded) <= ownCreateTimeEpsilon:
			return boolPtr(true)
		}
		integral := !math.IsInf(*recorded, 0) && math.Trunc(*recorded) == *recorded
		return boolPtr(integral && 0 <= *w.CT-*recorded && *w.CT-*recorded < wholeSecondCT)
	}
	if !w.Alive(pid) {
		return boolPtr(false)
	}
	if recorded == nil {
		return nil
	}
	actual := w.CTOf(pid)
	if actual == nil {
		return nil
	}
	return boolPtr(math.Abs(*actual-*recorded) <= CreateTimeToleranceSeconds)
}

// Whether the process identity pid and recordedCT runs now, nil when alive but unprovable
func IncarnationLive(pid int, recordedCT any) *bool {
	return incarnation(pid, asCT(recordedCT), realWorld())
}

// Liveness of one marker identity, an unprovable one bounded by the legacy marker age ceiling
func identityLive(pid int, createTime *float64, age float64, w World) bool {
	if verdict := incarnation(pid, createTime, w); verdict != nil {
		return *verdict
	}
	return age <= UpdateMarkerMaxAgeSeconds
}

// Marker line three for pid, empty when its creation time is unreadable
func IdentityLine(pid int) string {
	ct := ProcessCreateTime(pid)
	if ct == nil {
		return ""
	}
	return fmt.Sprintf("ct:%.3f", *ct)
}

type marker struct {
	raw                []byte
	pid                int
	startedAt          *uint64
	createTime         *float64
	delegatePID        *int
	delegateCreateTime *float64
	inFlight           bool
	ctText             *string
	delegateCTText     *string
	runs               []string
}

func (m marker) run() *string {
	if len(m.runs) == 0 {
		return nil
	}
	return &m.runs[0]
}

func (m marker) age(w World) float64 {
	if m.startedAt == nil {
		return math.Inf(1)
	}
	return w.Now - float64(*m.startedAt)
}

func (m marker) ownerLive(w World) bool {
	return m.startedAt != nil && identityLive(m.pid, m.createTime, m.age(w), w)
}

func (m marker) delegateLive(w World) bool {
	return m.delegatePID != nil && m.startedAt != nil && identityLive(*m.delegatePID, m.delegateCreateTime, m.age(w), w)
}

// Live identity of the claim, zero for an in flight empty marker, nil when every identity is dead
func (m marker) livePID(w World) *int {
	if m.inFlight {
		zero := 0
		return &zero
	}
	if m.ownerLive(w) {
		pid := m.pid
		return &pid
	}
	if m.delegateLive(w) {
		return m.delegatePID
	}
	return nil
}

// Claim rewritten to pid from the same start, run lines kept
func (m marker) canonical(pid int, ctText *string) []byte {
	body := fmt.Sprintf("%d\n%d\n", pid, *m.startedAt)
	if ctText != nil && *ctText != "" {
		body += "ct:" + *ctText + "\n"
	}
	for _, run := range m.runs {
		body += "run:" + run + "\n"
	}
	return []byte(body)
}

// ASCII digits whose value fits limit, counted before conversion so oversized fields stay malformed
func boundedInt(text string, limit uint64) (uint64, bool) {
	if !intLineRE.MatchString(text) {
		return 0, false
	}
	significant := strings.TrimLeft(text, "0")
	if significant == "" {
		significant = "0"
	}
	if len(significant) > len(strconv.FormatUint(limit, 10)) {
		return 0, false
	}
	value, err := strconv.ParseUint(significant, 10, 64)
	if err != nil || value > limit {
		return 0, false
	}
	return value, true
}

// Parses marker bytes under the shared corpus contract, mtime marking a young empty marker in flight
func parseMarker(raw []byte, mtime *float64) marker {
	text := strings.TrimPrefix(pystr.DecodeUTF8Replace(raw), "\ufeff")
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, strings.Trim(strings.TrimSuffix(line, "\r"), " \t"))
	}
	for len(lines) < 3 {
		lines = append(lines, "")
	}
	m := marker{raw: raw, pid: -1}
	if pid, ok := boundedInt(lines[0], u32Max); ok {
		m.pid = int(pid)
		if started, ok := boundedInt(lines[1], u64Max); ok {
			m.startedAt = &started
		}
	}
	if match := ctLineRE.FindStringSubmatch(lines[2]); match != nil {
		value, _ := strconv.ParseFloat(match[1], 64)
		m.createTime, m.ctText = &value, &match[1]
	}
	for _, line := range lines[3:] {
		if match := delegateLineRE.FindStringSubmatch(line); match != nil && m.delegatePID == nil {
			if pid, ok := boundedInt(match[1], u32Max); ok {
				delegate := int(pid)
				value, _ := strconv.ParseFloat(match[2], 64)
				m.delegatePID, m.delegateCreateTime, m.delegateCTText = &delegate, &value, &match[2]
			}
		}
		if match := runLineRE.FindStringSubmatch(line); match != nil {
			m.runs = append(m.runs, match[1])
		}
	}
	m.inFlight = len(raw) == 0 && mtime != nil && float64(time.Now().UnixNano())/1e9-*mtime < EmptyMarkerGraceSeconds
	return m
}

// Verdict malformed, dead, ours or live of marker bytes with the live owner and the first run id
func JudgeMarker(raw []byte, w *World) (string, *int, *string) {
	world := realWorld()
	if w != nil {
		world = *w
	}
	m := parseMarker(raw, nil)
	if m.startedAt == nil {
		return "malformed", nil, nil
	}
	owner := m.livePID(world)
	if owner == nil {
		return "dead", nil, m.run()
	}
	ours := (m.pid == world.PID && m.ownerLive(world)) || (m.delegatePID != nil && *m.delegatePID == world.PID && m.delegateLive(world))
	if ours {
		return "ours", owner, m.run()
	}
	return "live", owner, m.run()
}

// What releasing the world's claim does to marker bytes, delete, rewrite with new bytes, or keep
func releaseDecision(raw []byte, w *World) (string, []byte) {
	world := realWorld()
	if w != nil {
		world = *w
	}
	m := parseMarker(raw, nil)
	if m.startedAt == nil {
		return "keep", nil
	}
	if m.pid == world.PID && m.ownerLive(world) {
		if m.delegatePID != nil && *m.delegatePID != world.PID && m.delegateLive(world) {
			return "rewrite", m.canonical(*m.delegatePID, m.delegateCTText)
		}
		return "delete", nil
	}
	if m.delegatePID != nil && *m.delegatePID == world.PID && m.delegateLive(world) {
		if m.ownerLive(world) {
			return "rewrite", m.canonical(m.pid, m.ctText)
		}
		return "delete", nil
	}
	return "keep", nil
}

// Marker bytes at path, nil on any OSError
func readMarkerBytes(path string) []byte {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if raw == nil {
		raw = []byte{}
	}
	return raw
}

// Parsed marker at path, an empty one carrying its mtime, nil when unreadable
func readMarker(path string) *marker {
	raw := readMarkerBytes(path)
	if raw == nil {
		return nil
	}
	var mtime *float64
	if len(raw) == 0 {
		if info, err := os.Stat(path); err == nil {
			value := float64(info.ModTime().UnixNano()) / 1e9
			mtime = &value
		}
	}
	m := parseMarker(raw, mtime)
	return &m
}

// Joins rel under base the way a pathlib division does, an absolute rel replacing base
func joinPath(base, rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(base, rel)
}

// Reads a small text file as utf-8-sig, absent on an OSError and the decode error raised
func readGitPointer(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, nil
	}
	text, err := pystr.DecodeUTF8Sig(data)
	if err != nil {
		return "", false, err
	}
	return pystr.Strip(text), true, nil
}

// Common git dir of the checkout at root read from disk, empty when root is no checkout
func GitCommonDir(root string) (string, error) {
	dot := filepath.Join(root, ".git")
	info, err := os.Stat(dot)
	if err != nil {
		return "", nil
	}
	var gitdir string
	switch {
	case info.IsDir():
		gitdir = dot
	case info.Mode().IsRegular():
		text, ok, err := readGitPointer(dot)
		if !ok || err != nil || !strings.HasPrefix(text, "gitdir:") {
			return "", err
		}
		gitdir = joinPath(root, pystr.Strip(text[len("gitdir:"):]))
	default:
		return "", nil
	}
	common := filepath.Join(gitdir, "commondir")
	if info, err := os.Stat(common); err == nil && info.Mode().IsRegular() {
		text, ok, err := readGitPointer(common)
		if !ok || err != nil {
			return "", err
		}
		gitdir = joinPath(gitdir, text)
	}
	return filepath.Clean(gitdir), nil
}
