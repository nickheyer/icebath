package cron

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Module attributes the chronos tests patch, config reads, job reads and the fallback ticker start
var (
	chronosCfg = func(s *Store, keys ...string) any {
		cfg, err := s.loadConfig()
		if err != nil {
			return ""
		}
		if v, ok := config.CfgGet(map[string]any(cfg), keys...); ok {
			return v
		}
		return ""
	}
	chronosLoadJobs = func(ctx context.Context, s *Store) ([]*Job, error) { return s.LoadJobs(home.WithOverride(ctx, s.home)) }
	chronosGetJob   = func(ctx context.Context, s *Store, jobID string) (*Job, error) {
		return s.getJob(home.WithOverride(ctx, s.home), jobID)
	}
	chronosFallbackStart = func(ctx context.Context, stop <-chan struct{}, o StartOptions) {
		(&InProcessCronScheduler{}).Start(ctx, stop, o)
	}
	chronosAuthState = func(s *Store) (map[string]any, error) {
		return provider.NewStore(s.home).GetProviderAuthState("nous")
	}
)

// ChronosCronScheduler arms one NAS one shot per job and lets NAS call the agent back at fire time
type ChronosCronScheduler struct {
	s                *Store
	mu               sync.Mutex
	armed            map[string]*string
	armedOrder       []string
	client           chronosClient
	identityRejected bool
	stop             <-chan struct{}
	started          bool
	startCtx         context.Context
	tickerOpts       StartOptions
}

func newChronosProvider(s *Store) *ChronosCronScheduler {
	return &ChronosCronScheduler{s: s, armed: map[string]*string{}}
}

func (*ChronosCronScheduler) Name() string { return "chronos" }

// Configured portal and callback plus a stored Nous access token, without any network call
func (c *ChronosCronScheduler) IsAvailable() bool {
	if !pyTruthy(chronosCfg(c.s, "cron", "chronos", "portal_url")) || !pyTruthy(chronosCfg(c.s, "cron", "chronos", "callback_url")) {
		return false
	}
	state, err := chronosAuthState(c.s)
	if err != nil {
		return false
	}
	return pyTruthy(state["access_token"])
}

func (c *ChronosCronScheduler) getClient() (chronosClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		portal := chronosCfg(c.s, "cron", "chronos", "portal_url")
		text, ok := portal.(string)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'rstrip'", pyrepr.TypeName(portal))
		}
		c.client = newNASCronClient(c.s, text)
	}
	return c.client, nil
}

func (c *ChronosCronScheduler) reconcileLogged(debug bool, what string) {
	if err := c.Reconcile(); err != nil {
		msg := fmt.Sprintf("Chronos %s reconcile failed: %v", what, err)
		if debug {
			c.s.log.Debug(msg)
		} else {
			c.s.log.Warn(msg)
		}
	}
}

// Recovers interrupted attempts then reconciles the armed one shots against the jobs store
func (c *ChronosCronScheduler) Start(ctx context.Context, stop <-chan struct{}, o StartOptions) {
	interval := 60.0
	if o.Interval != nil {
		interval = *o.Interval
	}
	c.mu.Lock()
	c.tickerOpts = StartOptions{Adapters: o.Adapters, Loop: o.Loop, Interval: &interval}
	c.stop, c.started, c.startCtx = stop, true, context.WithoutCancel(ctx)
	c.mu.Unlock()
	if _, err := schedulerRecoverInterrupted(ctx, c); err != nil {
		c.s.log.Error("Exception in cron scheduler thread: " + tickErrorText(err))
		return
	}
	c.reconcileLogged(false, "start()")
}

func (*ChronosCronScheduler) Stop() {}

func (c *ChronosCronScheduler) OnJobsChanged() error {
	c.mu.Lock()
	rejected := c.identityRejected
	c.mu.Unlock()
	if !rejected {
		c.reconcileLogged(true, "on_jobs_changed")
	}
	return nil
}

// Arms only the created job, an identity rejection hands its fire to the built-in ticker
func (c *ChronosCronScheduler) RegisterJob(job *Job) error {
	err := c.armOneShot(job)
	var nasErr *NasCronClientError
	if errors.As(err, &nasErr) && nasErr.IdentityRejected() {
		c.noteIdentityRejected()
		return nil
	}
	return err
}

func (c *ChronosCronScheduler) armOneShot(job *Job) error {
	c.mu.Lock()
	rejected := c.identityRejected
	c.mu.Unlock()
	if rejected || job.NextRunAt == nil || *job.NextRunAt == "" {
		return nil
	}
	fireAt := *job.NextRunAt
	client, err := c.getClient()
	if err != nil {
		return err
	}
	callback := chronosCfg(c.s, "cron", "chronos", "callback_url")
	callbackText := ""
	if pyTruthy(callback) {
		callbackText = pyrepr.Str(callback)
	}
	if _, err := client.Provision(job.ID, fireAt, callbackText, job.ID+":"+fireAt); err != nil {
		return err
	}
	c.mu.Lock()
	c.setArmed(job.ID, &fireAt)
	c.mu.Unlock()
	return nil
}

func (c *ChronosCronScheduler) setArmed(jobID string, fireAt *string) {
	if _, ok := c.armed[jobID]; !ok {
		c.armedOrder = append(c.armedOrder, jobID)
	}
	c.armed[jobID] = fireAt
}

func (c *ChronosCronScheduler) dropArmed(jobID string) {
	if _, ok := c.armed[jobID]; !ok {
		return
	}
	delete(c.armed, jobID)
	for i, id := range c.armedOrder {
		if id == jobID {
			c.armedOrder = append(c.armedOrder[:i:i], c.armedOrder[i+1:]...)
			break
		}
	}
}

// Latches the rejection once, warns, and starts the built-in ticker when this process runs a scheduler
func (c *ChronosCronScheduler) noteIdentityRejected() {
	c.mu.Lock()
	if c.identityRejected {
		c.mu.Unlock()
		return
	}
	c.identityRejected = true
	started, stop, ctx, opts := c.started, c.stop, c.startCtx, c.tickerOpts
	c.mu.Unlock()
	c.s.log.Warn("Chronos: NAS rejected this agent's Nous credential for agent-cron (403 invalid_client). " +
		"The Nous token in auth.json is not this instance's provisioned identity (an agent:* client " +
		"or the hosted bootstrap session), so no job can be armed. A normal `icebath auth` re-login " +
		"cannot fix this; the hosted credential has to be restored from the Nous Portal. Falling back " +
		"to the built-in cron ticker for this process so scheduled jobs keep firing on time.")
	if !started {
		return
	}
	go chronosFallbackStart(ctx, stop, opts)
}

func (c *ChronosCronScheduler) armLogged(job *Job, what string) {
	err := c.armOneShot(job)
	if err == nil {
		return
	}
	var nasErr *NasCronClientError
	if errors.As(err, &nasErr) && nasErr.IdentityRejected() {
		c.noteIdentityRejected()
		return
	}
	c.s.log.Warn(fmt.Sprintf("Chronos failed to %s: %v", what, err))
}

func (c *ChronosCronScheduler) cancel(jobID string) error {
	client, err := c.getClient()
	if err == nil {
		_, err = client.Cancel(jobID)
	}
	c.mu.Lock()
	c.dropArmed(jobID)
	c.mu.Unlock()
	return err
}

// Armed job ids and fire times, the local cache first, else what NAS reports, empty on a failed read
func (c *ChronosCronScheduler) listArmed() ([]string, map[string]*string) {
	c.mu.Lock()
	if len(c.armed) > 0 {
		order := append([]string(nil), c.armedOrder...)
		snapshot := make(map[string]*string, len(c.armed))
		for k, v := range c.armed {
			snapshot[k] = v
		}
		c.mu.Unlock()
		return order, snapshot
	}
	c.mu.Unlock()
	order, observed, err := c.fetchArmed()
	if err != nil {
		c.s.log.Debug(fmt.Sprintf("Chronos _list_armed failed (will re-arm idempotently): %v", err))
		return nil, map[string]*string{}
	}
	c.mu.Lock()
	for _, id := range order {
		c.setArmed(id, observed[id])
	}
	c.mu.Unlock()
	return order, observed
}

func (c *ChronosCronScheduler) fetchArmed() ([]string, map[string]*string, error) {
	client, err := c.getClient()
	if err != nil {
		return nil, nil, err
	}
	items, err := client.ListArmed()
	if err != nil {
		return nil, nil, err
	}
	var order []string
	observed := map[string]*string{}
	for _, raw := range items {
		item := config.AsMap(raw)
		if item == nil {
			return nil, nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))
		}
		id := item["job_id"]
		if !pyTruthy(id) {
			continue
		}
		key, ok := id.(string)
		if !ok {
			key = pyrepr.Repr(id)
		}
		var fireAt *string
		if v, present := item["fire_at"]; !present {
			empty := ""
			fireAt = &empty
		} else if text, isText := v.(string); isText {
			fireAt = &text
		}
		if _, seen := observed[key]; !seen {
			order = append(order, key)
		}
		observed[key] = fireAt
	}
	return order, observed, nil
}

// Arms every enabled unpaused job whose armed fire time differs and cancels armed orphans
func (c *ChronosCronScheduler) Reconcile() error {
	jobs, err := chronosLoadJobs(c.startCtx, c.s)
	if err != nil {
		return err
	}
	var desiredOrder []string
	desired := map[string]string{}
	for _, j := range jobs {
		if j.Enabled && j.NextRunAt != nil && *j.NextRunAt != "" && j.State != "paused" {
			if _, seen := desired[j.ID]; !seen {
				desiredOrder = append(desiredOrder, j.ID)
			}
			desired[j.ID] = *j.NextRunAt
		}
	}
	observedOrder, observed := c.listArmed()
	for _, jobID := range desiredOrder {
		fireAt := desired[jobID]
		if cur, ok := observed[jobID]; ok && cur != nil && *cur == fireAt {
			continue
		}
		job, err := chronosGetJob(c.startCtx, c.s, jobID)
		if err != nil {
			return err
		}
		if job != nil {
			c.armLogged(job, "arm job "+jobID)
		}
	}
	for _, jobID := range observedOrder {
		if _, want := desired[jobID]; want {
			continue
		}
		if err := c.cancel(jobID); err != nil {
			c.s.log.Warn(fmt.Sprintf("Chronos failed to cancel orphan %s: %v", jobID, err))
		}
	}
	return nil
}

// Runs the claimed job through the base dispatcher then re-arms its next one shot when it still has one
func (c *ChronosCronScheduler) FireClaimed(ctx context.Context, job *Job, o FireOptions) (bool, error) {
	ran, err := baseFireClaimed(ctx, c, job, o)
	if err != nil || !ran {
		return ran, err
	}
	current, err := chronosGetJob(ctx, c.s, job.ID)
	if err != nil {
		return ran, err
	}
	if current != nil && current.Enabled && current.NextRunAt != nil && *current.NextRunAt != "" {
		c.armLogged(current, fmt.Sprintf("re-arm job %s after fire", job.ID))
	}
	return ran, nil
}
