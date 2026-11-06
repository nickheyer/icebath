package observability

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
)

func named(metric string, dimensions map[string]string, ok bool) (Counter, bool) {
	if !ok {
		return Counter{}, false
	}
	return Counter{Metric: metric, Dimensions: dimensions}, true
}

// Contract projections of one event, every match recorded
var subscriberCounters = []func(*RelayEvent) (Counter, bool){
	ClientActiveCounter,
	InstallSnapshotCounter,
	func(e *RelayEvent) (Counter, bool) {
		d, ok := ModelCallDimensions(e)
		return named(ModelRouteMetric, d, ok)
	},
	func(e *RelayEvent) (Counter, bool) {
		d, ok := ToolCallDimensions(e)
		return named(ToolCallMetric, d, ok)
	},
	func(e *RelayEvent) (Counter, bool) {
		d, ok := ToolUsageDimensions(e)
		return named(ToolUsageMetric, d, ok)
	},
	func(e *RelayEvent) (Counter, bool) {
		d, ok := ToolLatencyDimensions(e)
		return named(ToolLatencyMetric, d, ok)
	},
	TaskCounter,
	TaskDurationCounter,
	ToolApprovalCounter,
	SkillCounter,
	DecisionCounter,
	FeatureUsedCounter,
}

// SharedMetricsSubscriber persists validated counters from Relay lifecycle events
type SharedMetricsSubscriber struct {
	Store          *SharedMetricsStore
	clientResource map[string]string
	runtimeID      *string
	mu             sync.Mutex
	active         bool
	milestonesDone ValueSet
	featuresDone   ValueSet
	savedTickets   map[string]int
	homeDir        string
}

// Subscriber bound to the calling profile home, optionally accepting one Relay runtime's events only
func NewSharedMetricsSubscriber(ctx context.Context, store *SharedMetricsStore, version string, runtimeID *string) (*SharedMetricsSubscriber, error) {
	method, err := DetectClientInstallMethod(ctx)
	if err != nil {
		return nil, err
	}
	milestones, err := store.RecordedMilestones(ctx)
	if err != nil {
		return nil, err
	}
	features, err := store.RecordedFeatures(ctx)
	if err != nil {
		return nil, err
	}
	return &SharedMetricsSubscriber{
		Store:          store,
		clientResource: ClientResource(version, pyplatform.System(), pyplatform.Machine(), method),
		runtimeID:      runtimeID,
		active:         true,
		milestonesDone: milestones.Union(),
		featuresDone:   features.Union(),
		savedTickets:   map[string]int{},
		homeDir:        home.Dir(ctx),
	}, nil
}

// Stops accepting events before telemetry is disabled or torn down
func (s *SharedMetricsSubscriber) Deactivate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = false
}

// Every metric, dimensions and amount the event satisfies
func classify(event *RelayEvent) []ClassifiedCounter {
	var out []ClassifiedCounter
	for _, project := range subscriberCounters {
		if c, ok := project(event); ok {
			out = append(out, ClassifiedCounter{c.Metric, c.Dimensions, 1})
		}
	}
	for _, sum := range ModelTokenCounters(event) {
		out = append(out, ClassifiedCounter{sum.Metric, sum.Dimensions, sum.Amount})
	}
	return out
}

func (s *SharedMetricsSubscriber) recordMilestones(ctx context.Context, metric string, dimensions map[string]string) error {
	var reached []string
	for _, m := range MilestonesFor(metric, dimensions) {
		if !s.milestonesDone.Has(m) {
			reached = append(reached, m)
		}
	}
	if len(reached) == 0 {
		return nil
	}
	age, err := InstallAgeBucketAt(ctx, s.homeDir)
	if err != nil {
		return err
	}
	for _, milestone := range reached {
		if _, err := s.Store.RecordMilestone(ctx, milestone, age, s.clientResource); err != nil {
			return err
		}
		s.milestonesDone[milestone] = struct{}{}
	}
	return nil
}

func (s *SharedMetricsSubscriber) recordEngagement(ctx context.Context, event *RelayEvent, classified []ClassifiedCounter) {
	surface := InteractionSurface(classified)
	route, hasRoute := TurnRoute(event)
	if surface == "" && !hasRoute {
		return
	}
	var routeArg *[2]string
	if hasRoute {
		routeArg = &route
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	if err := guardedCall(func() error { return Record(ctx, s.Store, s.clientResource, surface, routeArg) }); err != nil {
		slog.Warn("Unable to update the Icebath engagement rollup", "error", err)
	}
}

func (s *SharedMetricsSubscriber) persist(ctx context.Context, metric string, dimensions map[string]string, amount int64) error {
	var err error
	switch metric {
	case FeatureUsedMark:
	case FeatureDisabledMetric:
		_, err = s.Store.RecordCounterOncePerDay(ctx, metric, dimensions, s.clientResource)
	case ClientActiveMetric:
		_, err = s.Store.RecordClientActive(ctx, s.clientResource)
	case InstallSnapshotMetric:
		_, err = s.Store.RecordInstallSnapshot(ctx, dimensions, s.clientResource)
	default:
		err = s.Store.RecordCounter(ctx, metric, dimensions, s.clientResource, amount)
	}
	return err
}

func (s *SharedMetricsSubscriber) recordFeatures(ctx context.Context, metric string, dimensions map[string]string) {
	var reached []string
	for _, f := range FeaturesFor(metric, dimensions) {
		if !s.featuresDone.Has(f) {
			reached = append(reached, f)
		}
	}
	if len(reached) == 0 {
		return
	}
	age := DaysSinceInstallBucket(ctx, s.homeDir)
	for _, feature := range reached {
		if _, err := s.Store.RecordFeatureAdoption(ctx, feature, age, s.clientResource); err != nil {
			slog.Debug("Feature adoption not latched: "+feature, "error", err)
			continue
		}
		s.featuresDone[feature] = struct{}{}
	}
}

// Number of events carrying the ticket that settled without a store error, forgetting the ticket
func (s *SharedMetricsSubscriber) TakeSaved(ticket string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.savedTickets[ticket]
	delete(s.savedTickets, ticket)
	return n
}

// Runs one step turning a panic into an error, the except Exception around each store write
func guardedCall(step func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return step()
}

// Persists every counter one Relay event satisfies and settles its commit ticket
func (s *SharedMetricsSubscriber) Call(ctx context.Context, event *RelayEvent) {
	var metadata map[string]any
	isDict := false
	if event != nil {
		metadata, isDict = asDict(event.Metadata)
	}
	if s.runtimeID != nil {
		if id, ok := metadata[relayRuntimeInstanceKey].(string); !isDict || !ok || id != *s.runtimeID {
			return
		}
	}
	ticket := ""
	if isDict {
		ticket, _ = metadata[CommitTicketKey].(string)
	}
	saved := true
	classified := classify(event)
	s.recordEngagement(ctx, event, classified)
	for _, row := range classified {
		if !s.persistOne(ctx, row) {
			if !s.isActive() {
				return
			}
			saved = false
		}
	}
	if ticket != "" && saved {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.active {
			s.savedTickets[ticket]++
		}
	}
}

func (s *SharedMetricsSubscriber) isActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Persists one counter with its milestones and features, false when inactive or a store write failed
func (s *SharedMetricsSubscriber) persistOne(ctx context.Context, row ClassifiedCounter) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return false
	}
	err := guardedCall(func() error {
		if err := s.persist(ctx, row.Metric, row.Dimensions, row.Amount); err != nil {
			return err
		}
		if err := s.recordMilestones(ctx, row.Metric, row.Dimensions); err != nil {
			return err
		}
		s.recordFeatures(ctx, row.Metric, row.Dimensions)
		return nil
	})
	if err != nil {
		slog.Warn("Unable to persist the Icebath shared metric: "+row.Metric, "error", err)
		return false
	}
	return true
}
