package updater

import (
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	ownCTEpsilonS        = 0.005
	createTimeToleranceS = 2.0
	v1MaxAgeS            = 1200
	u32Max               = 0xffffffff
	emptyMarkerGraceMS   = 5000
	markerFileName       = ".icebath-update-in-progress"
	identityOurs         = "ours"
	identityMatch        = "match"
	identityUnknown      = "unknown"
	identityDead         = "dead"
	verdictMalformed     = "malformed"
	verdictDead          = "dead"
	verdictOurs          = "ours"
	verdictLive          = "live"
	inspectionAbsent     = "absent"
	inspectionLive       = "live"
	inspectionOurs       = "ours"
	inspectionDead       = "dead"
)

var (
	markerIntRE      = regexp.MustCompile(`^[0-9]+$`)
	markerCTRE       = regexp.MustCompile(`^ct:([0-9]+(?:\.[0-9]+)?)$`)
	markerDelegateRE = regexp.MustCompile(`^delegate:([0-9]+) ct:([0-9]+(?:\.[0-9]+)?)$`)
	markerRunRE      = regexp.MustCompile(`^run:([A-Za-z0-9._-]{1,128})$`)
	runIDRE          = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	u64Max           = new(big.Int).SetUint64(math.MaxUint64)
)

// Process facts the marker judge reads, the live process table in production
var (
	pidAlive       = func(pid int) bool { return backend.IsPidAlive(pid) && !proc.Zombie(pid) }
	processCreated = func(pid int) *float64 { return updaterecord.ProcessCreateTime(pid) }
	currentPID     = os.Getpid
	nowMillis      = func() float64 { return float64(time.Now().UnixNano()) / 1e6 }
)

// MarkerDelegate is the delegate identity line of a marker
type MarkerDelegate struct {
	PID int
	CT  float64
}

// UpdateMarker is the positional parse of the shared update marker
type UpdateMarker struct {
	PID       int
	StartedAt float64
	CT        *float64
	Delegate  *MarkerDelegate
	Run       *string
}

// MarkerJudgement is the verdict over one marker body with the identity states it read
type MarkerJudgement struct {
	Verdict       string
	Owner         *int
	Run           *string
	Marker        *UpdateMarker
	OwnerState    string
	DelegateState string
}

// JudgeEnv holds the process facts the identity rule consults
type JudgeEnv struct {
	OurPID     int
	OurCT      func() *float64
	IsAlive    func(pid int) bool
	CreateTime func(pid int) *float64
	NowS       float64
}

// Marker file under a home
func markerPath(home string) string {
	return filepath.Join(home, markerFileName)
}

func cleanMarkerLine(line string) string {
	return strings.Trim(strings.TrimSuffix(line, "\r"), " \t")
}

func markerU32(text string) (int, bool) {
	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil || n > u32Max {
		return 0, false
	}
	return int(n), true
}

// Positional parse identical in every reader, nil for a malformed body
func parseUpdateMarker(raw string) *UpdateMarker {
	lines := strings.Split(strings.TrimPrefix(raw, "\ufeff"), "\n")
	for i := range lines {
		lines[i] = cleanMarkerLine(lines[i])
	}
	if len(lines) < 2 || !markerIntRE.MatchString(lines[0]) || !markerIntRE.MatchString(lines[1]) {
		return nil
	}
	pid, ok := markerU32(lines[0])
	started, _ := new(big.Int).SetString(lines[1], 10)
	if !ok || started.Cmp(u64Max) > 0 {
		return nil
	}
	startedAt, _ := new(big.Float).SetInt(started).Float64()
	m := &UpdateMarker{PID: pid, StartedAt: startedAt}
	if len(lines) > 2 {
		if ct := markerCTRE.FindStringSubmatch(lines[2]); ct != nil {
			value, _ := strconv.ParseFloat(ct[1], 64)
			m.CT = &value
		}
	}
	if len(lines) > 3 {
		for _, line := range lines[3:] {
			if m.Delegate == nil {
				if d := markerDelegateRE.FindStringSubmatch(line); d != nil {
					if dpid, ok := markerU32(d[1]); ok {
						ct, _ := strconv.ParseFloat(d[2], 64)
						m.Delegate = &MarkerDelegate{PID: dpid, CT: ct}
						continue
					}
				}
			}
			if m.Run == nil {
				if r := markerRunRE.FindStringSubmatch(line); r != nil {
					run := r[1]
					m.Run = &run
				}
			}
		}
	}
	return m
}

// Identity state of one pid and recorded creation time under the A7 rule
func identityState(pid int, recordedCT *float64, startedAt float64, env JudgeEnv) string {
	if pid == 0 {
		return identityDead
	}
	if pid == env.OurPID {
		ourCT := env.OurCT()
		if recordedCT != nil && ourCT != nil && math.Abs(*recordedCT-*ourCT) <= ownCTEpsilonS {
			return identityOurs
		}
		return identityDead
	}
	if !env.IsAlive(pid) {
		return identityDead
	}
	var actual *float64
	if recordedCT != nil {
		actual = env.CreateTime(pid)
	}
	if recordedCT == nil || actual == nil {
		if env.NowS-startedAt <= v1MaxAgeS {
			return identityUnknown
		}
		return identityDead
	}
	if math.Abs(*recordedCT-*actual) <= createTimeToleranceS {
		return identityMatch
	}
	return identityDead
}

func isLiveIdentity(state string) bool {
	return state == identityOurs || state == identityMatch || state == identityUnknown
}

// Judges marker text against the process facts of env
func judgeMarkerText(text string, env JudgeEnv) MarkerJudgement {
	m := parseUpdateMarker(text)
	if m == nil {
		return MarkerJudgement{Verdict: verdictMalformed, OwnerState: identityDead}
	}
	ownerState := identityState(m.PID, m.CT, m.StartedAt, env)
	delegateState := ""
	if m.Delegate != nil {
		ct := m.Delegate.CT
		delegateState = identityState(m.Delegate.PID, &ct, m.StartedAt, env)
	}
	var owner *int
	if isLiveIdentity(ownerState) {
		pid := m.PID
		owner = &pid
	} else if isLiveIdentity(delegateState) {
		pid := m.Delegate.PID
		owner = &pid
	}
	verdict := verdictDead
	switch {
	case ownerState == identityOurs || delegateState == identityOurs:
		verdict = verdictOurs
	case owner != nil:
		verdict = verdictLive
	}
	return MarkerJudgement{Verdict: verdict, Owner: owner, Run: m.Run, Marker: m, OwnerState: ownerState, DelegateState: delegateState}
}

// Whether a live identity of the marker is some other process
func hasForeignLiveIdentity(j MarkerJudgement) bool {
	foreign := func(state string) bool { return state == identityMatch || state == identityUnknown }
	return foreign(j.OwnerState) || foreign(j.DelegateState)
}

// Creation time probe memoised per pid for the life of one wait
func cachedCreateTimeProbe(probe func(pid int) *float64) func(pid int) *float64 {
	var mu sync.Mutex
	memo := map[int]*float64{}
	seen := map[int]bool{}
	return func(pid int) *float64 {
		mu.Lock()
		if seen[pid] {
			hit := memo[pid]
			mu.Unlock()
			return hit
		}
		mu.Unlock()
		value := probe(pid)
		mu.Lock()
		if !seen[pid] {
			seen[pid], memo[pid] = true, value
		}
		hit := memo[pid]
		mu.Unlock()
		return hit
	}
}

// Host process facts as a judge environment, the given probe reading creation times
func hostJudgeEnv(createTime func(pid int) *float64, now func() float64) JudgeEnv {
	if createTime == nil {
		createTime = processCreated
	}
	if now == nil {
		now = nowMillis
	}
	ourPID := currentPID()
	return JudgeEnv{OurPID: ourPID, OurCT: func() *float64 { return createTime(ourPID) }, IsAlive: pidAlive, CreateTime: createTime, NowS: now() / 1000}
}

// MarkerInspection is the read only judgement of the marker file
type MarkerInspection struct {
	State     string
	Raw       []byte
	Marker    *UpdateMarker
	AgeMS     float64
	LivePID   int
	Judgement *MarkerJudgement
}

// Reads and judges the marker without touching it
func inspectUpdateMarker(home string, createTime func(pid int) *float64, now func() float64) MarkerInspection {
	if now == nil {
		now = nowMillis
	}
	file := markerPath(home)
	raw, err := os.ReadFile(file)
	if err != nil {
		return MarkerInspection{State: inspectionAbsent}
	}
	if len(raw) == 0 {
		ageMS := math.Inf(1)
		if info, err := os.Stat(file); err == nil {
			ageMS = now() - float64(info.ModTime().UnixNano())/1e6
		}
		if ageMS < emptyMarkerGraceMS {
			return MarkerInspection{State: inspectionLive, Raw: raw, AgeMS: math.Max(0, ageMS)}
		}
		return MarkerInspection{State: inspectionDead, Raw: raw}
	}
	judgement := judgeMarkerText(pystr.DecodeUTF8Replace(raw), hostJudgeEnv(createTime, now))
	m := judgement.Marker
	if m == nil || judgement.Verdict == verdictMalformed || judgement.Verdict == verdictDead {
		return MarkerInspection{State: inspectionDead, Raw: raw, Marker: m, Judgement: &judgement}
	}
	if hasForeignLiveIdentity(judgement) {
		livePID := m.PID
		if judgement.OwnerState != identityMatch && judgement.OwnerState != identityUnknown && m.Delegate != nil {
			livePID = m.Delegate.PID
		}
		return MarkerInspection{State: inspectionLive, Raw: raw, Marker: m, AgeMS: now() - m.StartedAt*1000, LivePID: livePID, Judgement: &judgement}
	}
	return MarkerInspection{State: inspectionOurs, Raw: raw, Marker: m, Judgement: &judgement}
}

// LiveUpdate is someone else's update that is running now
type LiveUpdate struct {
	PID       int
	OwnerPID  int
	AgeMS     float64
	StartedAt *float64
}

// Update another process is genuinely running, nil when none
func readLiveUpdateMarker(home string, createTime func(pid int) *float64) *LiveUpdate {
	inspection := inspectUpdateMarker(home, createTime, nil)
	if inspection.State != inspectionLive {
		return nil
	}
	live := &LiveUpdate{PID: inspection.LivePID, AgeMS: inspection.AgeMS}
	if inspection.Marker != nil {
		live.OwnerPID = inspection.Marker.PID
		started := inspection.Marker.StartedAt
		live.StartedAt = &started
	}
	return live
}
