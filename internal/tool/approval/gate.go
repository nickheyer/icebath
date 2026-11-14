package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/redact"
)

type BatchSlot interface {
	Consume(command, envKind string, hostAccess bool) (tool.Decision, bool)
	Preparing() bool
	Register(withdraw func()) error
	Published()
}

type batchSlotKey struct{}

func WithBatchSlot(ctx context.Context, s BatchSlot) context.Context {
	return context.WithValue(ctx, batchSlotKey{}, s)
}

func batchSlot(ctx context.Context) BatchSlot {
	s, _ := ctx.Value(batchSlotKey{}).(BatchSlot)
	return s
}

type GateOptions struct {
	Config       *config.Store
	Homes        *home.Resolver
	Redactor     *redact.Redactor
	Side         tool.SideModel
	Hooks        tool.Hooks
	Transports   func() []Transport
	YoloFrozen   bool
	Translate    func(key string, args map[string]string) string
	SkipGuards   func(envKind string) bool
	YoloChanged  func(ctx context.Context, sessionKey string) error
	SessionEnded func(ctx context.Context, sessionKey string) error
	SessionMoved func(ctx context.Context, oldKey, newKey string) error
	SkillDir     func(ctx context.Context, name string) (string, bool, error)
	FoldPatch    func(base, old, replacement string, replaceAll bool) (string, error)
}

type Gate struct {
	o              GateOptions
	protector      *Protector
	guardian       guardian
	transportSlots chan struct{}

	mu          sync.Mutex
	detectors   map[string]*Detector
	approved    map[string]map[string]bool
	yolo        map[string]bool
	launched    map[string]bool
	permanent   map[string]map[string]bool
	baseline    map[string]map[string]bool
	denials     map[string]int
	denialOrder []string
	queues      map[string][]*waiter
	waits       map[string]*waitState
	waitOrder   []string
}

func NewGate(o GateOptions) (*Gate, error) {
	for _, req := range []struct {
		name   string
		filled bool
	}{
		{"approval config store", o.Config != nil},
		{"approval home resolver", o.Homes != nil},
		{"approval redactor", o.Redactor != nil},
		{"approval side model", o.Side != nil},
		{"approval hooks", o.Hooks != nil},
		{"approval transports", o.Transports != nil},
		{"approval message catalog", o.Translate != nil},
		{"container guard flags", o.SkipGuards != nil},
		{"permission mode dependents", o.YoloChanged != nil},
		{"session kernel shutdown", o.SessionEnded != nil},
		{"session spend transfer", o.SessionMoved != nil},
		{"skill directory lookup", o.SkillDir != nil},
		{"skill patch folding", o.FoldPatch != nil},
	} {
		if !req.filled {
			return nil, &tool.UnfilledError{Name: req.name}
		}
	}
	protector, err := NewProtector()
	if err != nil {
		return nil, err
	}
	return &Gate{
		o:              o,
		protector:      protector,
		guardian:       guardian{side: o.Side, config: o.Config, redactor: o.Redactor, hooks: o.Hooks},
		transportSlots: make(chan struct{}, maxTransportWorkers),
		detectors:      map[string]*Detector{},
		approved:       map[string]map[string]bool{},
		yolo:           map[string]bool{},
		launched:       map[string]bool{},
		permanent:      map[string]map[string]bool{},
		baseline:       map[string]map[string]bool{},
		denials:        map[string]int{},
		queues:         map[string][]*waiter{},
		waits:          map[string]*waitState{},
	}, nil
}

func newRequestID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (g *Gate) profileHome(ctx context.Context, c *tool.Call) (string, error) {
	if c != nil && c.Origin.ProfileHome != "" {
		return c.Origin.ProfileHome, nil
	}
	return g.o.Homes.Home(ctx)
}

func (g *Gate) Detector(ctx context.Context, c *tool.Call) (*Detector, error) {
	profileHome, err := g.profileHome(ctx, c)
	if err != nil {
		return nil, err
	}
	d, err := g.detectorFor(profileHome)
	if err != nil {
		return nil, err
	}
	if home.Override(ctx) != "" {
		return d, nil
	}
	launch, err := g.o.Homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	if launch != profileHome {
		return d, nil
	}
	for _, name := range []string{"ICEBATH_PROFILE_NAME", "ICEBATH_PROFILE"} {
		pin := pyStrip(os.Getenv(name))
		if pin == "" {
			continue
		}
		if pin == d.profile {
			return d, nil
		}
		pinned := *d
		pinned.profile = pin
		return &pinned, nil
	}
	return d, nil
}

func (g *Gate) detectorFor(profileHome string) (*Detector, error) {
	g.mu.Lock()
	d, ok := g.detectors[profileHome]
	g.mu.Unlock()
	if ok {
		return d, nil
	}
	d, err := NewDetector(profileHome)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if prior, ok := g.detectors[profileHome]; ok {
		return prior, nil
	}
	g.detectors[profileHome] = d
	return d, nil
}

func (g *Gate) Protector() *Protector { return g.protector }

func (g *Gate) Mode(ctx context.Context, o tool.Origin) config.ApprovalMode {
	return Mode(ctx, g.o.Config, o)
}

func (g *Gate) SetMode(ctx context.Context, o tool.Origin, requested string) ModeResult {
	return SetMode(ctx, g.o.Config, o, requested)
}

func (g *Gate) breakerThreshold(ctx context.Context) int {
	v := readApprovals(ctx, g.o.Config)
	if !v.loaded {
		return 3
	}
	return v.cfg.DenialBreakerThreshold
}

func (g *Gate) recordDenial(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	count := g.denials[key] + 1
	g.denialOrder = slices.DeleteFunc(g.denialOrder, func(k string) bool { return k == key })
	g.denials[key] = count
	g.denialOrder = append(g.denialOrder, key)
	for len(g.denialOrder) > 256 {
		delete(g.denials, g.denialOrder[0])
		g.denialOrder = g.denialOrder[1:]
	}
	return count
}

func (g *Gate) resetDenials(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.denials, key)
	g.denialOrder = slices.DeleteFunc(g.denialOrder, func(k string) bool { return k == key })
}

func (g *Gate) breakerAddendum(ctx context.Context, key string) string {
	g.mu.Lock()
	count := g.denials[key]
	g.mu.Unlock()
	threshold := g.breakerThreshold(ctx)
	if threshold <= 0 || count < threshold {
		return ""
	}
	slog.WarnContext(ctx, "Smart-approval circuit breaker tripped", "session", key, "denials", count, "threshold", threshold)
	return " CIRCUIT BREAKER: " + strconv.Itoa(count) + " consecutive commands were blocked by the security reviewer. " +
		"STOP attempting variations of this operation. Report the blocked operation to the user and either ask them " +
		"to run it manually or use /approve."
}

func (g *Gate) approveSession(sessionKey, key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	set := g.approved[sessionKey]
	if set == nil {
		set = map[string]bool{}
		g.approved[sessionKey] = set
	}
	set[key] = true
}

func (g *Gate) Yolo(sessionKey string) bool {
	if sessionKey == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.yolo[sessionKey]
}

func (g *Gate) setYolo(ctx context.Context, sessionKey string, on bool) {
	if sessionKey == "" {
		return
	}
	g.mu.Lock()
	if on {
		g.yolo[sessionKey] = true
	} else {
		delete(g.yolo, sessionKey)
	}
	g.mu.Unlock()
	g.releaseDependents(ctx, sessionKey)
}

func (g *Gate) releaseDependents(ctx context.Context, sessionKey string) {
	if err := g.o.YoloChanged(ctx, sessionKey); err != nil {
		slog.DebugContext(ctx, "Failed to release permission-mode dependent resources", "session", sessionKey, "error", err)
	}
}

func (g *Gate) ToggleYolo(ctx context.Context, sessionKey string, enabled *bool, persisted bool, persist func(bool) error) bool {
	on := false
	if enabled == nil {
		on = !(persisted || g.Yolo(sessionKey))
	} else {
		on = *enabled
	}
	if persist != nil {
		if err := persist(on); err != nil {
			slog.WarnContext(ctx, "failed to persist session yolo", "enabled", on, "session", sessionKey, "error", err)
		}
	}
	g.setYolo(ctx, sessionKey, on)
	return on
}

func (g *Gate) RestoreYolo(ctx context.Context, sessionKey string, persisted bool) bool {
	if sessionKey == "" || !persisted || g.o.YoloFrozen || g.Yolo(sessionKey) {
		return false
	}
	g.setYolo(ctx, sessionKey, true)
	return true
}

func (g *Gate) ApplyLaunchYolo(ctx context.Context, sessionKey string) {
	if sessionKey == "" || g.o.YoloFrozen {
		return
	}
	g.mu.Lock()
	if g.launched[sessionKey] {
		g.mu.Unlock()
		return
	}
	g.launched[sessionKey] = true
	g.mu.Unlock()
	g.setYolo(ctx, sessionKey, true)
}

func (g *Gate) RestoreGatewayYolo(ctx context.Context, sessionKey string, persisted *bool, launch bool) {
	if sessionKey == "" {
		return
	}
	if persisted == nil {
		if launch {
			g.ApplyLaunchYolo(ctx, sessionKey)
		}
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.launched[sessionKey] {
		return
	}
	g.launched[sessionKey] = true
	if *persisted {
		g.yolo[sessionKey] = true
	} else {
		delete(g.yolo, sessionKey)
	}
}

func (g *Gate) TransferYolo(ctx context.Context, oldKey, newKey string) error {
	if err := g.o.SessionMoved(ctx, oldKey, newKey); err != nil {
		return err
	}
	if oldKey == "" || newKey == "" || oldKey == newKey || !g.Yolo(oldKey) {
		return nil
	}
	g.setYolo(ctx, newKey, true)
	g.setYolo(ctx, oldKey, false)
	return nil
}

func (g *Gate) ClearSession(ctx context.Context, sessionKey string) {
	if sessionKey == "" {
		return
	}
	g.mu.Lock()
	delete(g.approved, sessionKey)
	delete(g.yolo, sessionKey)
	delete(g.launched, sessionKey)
	for _, w := range g.queues[sessionKey] {
		w.cancelled = "the session ended before the prompt was answered"
		w.wake()
	}
	delete(g.queues, sessionKey)
	g.mu.Unlock()
	g.releaseDependents(ctx, sessionKey)
	if err := g.o.SessionEnded(ctx, sessionKey); err != nil {
		slog.DebugContext(ctx, "Failed to shut down session kernels", "session", sessionKey, "error", err)
	}
}

func (g *Gate) permanentLocked(ctx context.Context) map[string]bool {
	key := home.Override(ctx)
	set := g.permanent[key]
	if set == nil {
		set = map[string]bool{}
		g.permanent[key] = set
	}
	cfg, err := g.o.Config.LoadReadonly(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Failed to load permanent allowlist", "error", err)
		return set
	}
	onDisk := map[string]bool{}
	for _, entry := range cfg.CommandAllowlist {
		onDisk[entry] = true
	}
	base := g.baseline[key]
	if sameSet(onDisk, base) {
		return set
	}
	for entry := range base {
		if !onDisk[entry] {
			delete(set, entry)
		}
	}
	for entry := range onDisk {
		if !base[entry] {
			set[entry] = true
		}
	}
	g.baseline[key] = onDisk
	return set
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func (g *Gate) Permanent(ctx context.Context) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	set := g.permanentLocked(ctx)
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (g *Gate) Approved(ctx context.Context, c *tool.Call, key string) bool {
	aliases := KeyAliases(key)
	g.mu.Lock()
	defer g.mu.Unlock()
	permanent := g.permanentLocked(ctx)
	session := g.approved[sessionKey(c)]
	for _, a := range aliases {
		if permanent[a] || session[a] {
			return true
		}
	}
	return false
}

func (g *Gate) permanentlyApproved(ctx context.Context, key string) bool {
	aliases := KeyAliases(key)
	g.mu.Lock()
	defer g.mu.Unlock()
	permanent := g.permanentLocked(ctx)
	for _, a := range aliases {
		if permanent[a] {
			return true
		}
	}
	return false
}

func (g *Gate) persistChoice(ctx context.Context, sessionKey, choice string, keys []string) {
	for _, key := range keys {
		if choice != "session" && choice != "always" {
			continue
		}
		g.approveSession(sessionKey, key)
		if choice != "always" {
			continue
		}
		g.mu.Lock()
		set := g.permanentLocked(ctx)
		set[key] = true
		base := g.baseline[home.Override(ctx)]
		var grants []string
		for entry := range set {
			if !base[entry] {
				grants = append(grants, entry)
			}
		}
		g.mu.Unlock()
		g.savePermanent(ctx, grants)
	}
}

func (g *Gate) savePermanent(ctx context.Context, patterns []string) {
	cfg, err := g.o.Config.Load(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Could not save allowlist", "error", err)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := home.Override(ctx)
	base := g.baseline[key]
	merged := map[string]bool{}
	for _, entry := range cfg.CommandAllowlist {
		merged[entry] = true
	}
	for _, p := range patterns {
		if !base[p] {
			merged[p] = true
		}
	}
	sorted := make([]string, 0, len(merged))
	for entry := range merged {
		sorted = append(sorted, entry)
	}
	slices.Sort(sorted)
	if err := g.o.Config.WriteKey(ctx, "command_allowlist", sorted); err != nil {
		slog.WarnContext(ctx, "Could not save allowlist", "error", err)
		return
	}
	g.baseline[key] = merged
	set := map[string]bool{}
	for entry := range merged {
		set[entry] = true
	}
	g.permanent[key] = set
}

func (g *Gate) Bypass(ctx context.Context, c *tool.Call) bool {
	return g.o.YoloFrozen || g.Yolo(c.SessionKey) || Mode(ctx, g.o.Config, c.Origin) == config.ApprovalOff
}

func (g *Gate) yoloActive(c *tool.Call) bool {
	return g.o.YoloFrozen || g.Yolo(c.SessionKey) || c.Presence.Oneshot
}
