package cron

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const gatewayDownHint = "the gateway is not running, so agent jobs cannot fire; they run at the next tick once it is " +
	"up. Start it with `icebath gateway start` (or `icebath gateway install` for a service)."

const headlessProbeTimeout = 5 * time.Second

// Seam over the gateway endpoint probe the headless gate consults
var discoverGatewayEndpoint = gateway.DiscoverGatewayEndpoint

// Why a headless tick must hold its agent jobs, false when a ready owner serves the home
func gatewayUnavailable(ctx context.Context, h string) (string, bool) {
	if strings.TrimSpace(os.Getenv("ICEBATH_TUI_GATEWAY_URL")) != "" {
		return "", false
	}
	observed := discoverGatewayEndpoint(ctx, h, headlessProbeTimeout)
	switch observed.State {
	case "ready":
		return "", false
	case "absent":
		return gatewayDownHint, true
	}
	reason := observed.State
	if observed.ReasonCode != "" {
		reason += " (" + observed.ReasonCode + ")"
	}
	return fmt.Sprintf("the gateway is %s, so agent jobs cannot fire; they run at the next tick once it "+
		"is ready. Check it with `icebath gateway status`.", reason), true
}

// Stamps last_fire_error and drops the dispatch marks the due scan wrote for refused jobs
func (s *Store) noteFireRefused(jobIDs []string, detail string) error {
	ids := map[string]bool{}
	for _, id := range jobIDs {
		ids[id] = true
	}
	return s.withJobsLock(func(tx *jobsTx) error {
		jobs, err := s.loadJobs(tx)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if !ids[job.ID] {
				continue
			}
			job.LastFireError = &Stamp{At: s.clk.nowISO(), Detail: pystr.Slice(detail, 0, 500)}
			job.PendingSlot = nil
			if job.RunClaim != nil {
				job.RunClaim = nil
			}
		}
		return s.saveJobsLocked(tx, jobs, nil, false)
	})
}

// Due jobs a headless tick may dispatch, agent jobs held back while no ready gateway serves the home
func refuseAgentJobsWithoutGateway(ctx context.Context, s *Store, due []*Job) ([]*Job, error) {
	var agentIDs []string
	var scriptJobs []*Job
	for _, job := range due {
		if job.NoAgent {
			scriptJobs = append(scriptJobs, job)
		} else {
			agentIDs = append(agentIDs, job.ID)
		}
	}
	if len(agentIDs) == 0 {
		return due, nil
	}
	h := s.Home()
	hint, held := gatewayUnavailable(ctx, h)
	if !held {
		return due, nil
	}
	if err := s.noteFireRefused(agentIDs, "Skipped: "+hint); err != nil {
		return nil, err
	}
	s.log.Warn(fmt.Sprintf("Cron tick for %s skipped %d agent job(s) (%s): %s", h, len(agentIDs), strings.Join(agentIDs, ", "), hint))
	return scriptJobs, nil
}
