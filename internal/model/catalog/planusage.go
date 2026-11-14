package catalog

import (
	"context"
	"slices"
)

type PlanRead int

const (
	PlanCached PlanRead = iota
	PlanWarm
	PlanAnyAge
)

type PlanSnapshot interface {
	FeaturedModelIDs() []string
}

type PlanUsage interface {
	Snapshot(ctx context.Context, read PlanRead) (PlanSnapshot, bool)
	WindowPlanDefault(ctx context.Context, modelIDs []string, pricing *PriceTable) string
	LargeModelNotice(ctx context.Context, model, provider string) string
	ModelNotes(ctx context.Context, modelIDs []string) (labels map[string]string, planLine string)
}

func SplitRecommended(modelIDs, featured []string) (recommended, other []string) {
	recommended, other = []string{}, []string{}
	for _, id := range featured {
		if slices.Contains(modelIDs, id) && !slices.Contains(recommended, id) {
			recommended = append(recommended, id)
		}
	}
	for _, id := range modelIDs {
		if !slices.Contains(recommended, id) {
			other = append(other, id)
		}
	}
	return recommended, other
}

func FeaturedFirst(modelIDs, featured []string) []string {
	recommended, other := SplitRecommended(modelIDs, featured)
	return append(recommended, other...)
}

func (c *Catalog) featuredFirst(ctx context.Context, modelIDs []string, read PlanRead) ([]string, PlanSnapshot) {
	snapshot, ok := c.plan.Snapshot(ctx, read)
	if !ok || snapshot == nil {
		return modelIDs, nil
	}
	return FeaturedFirst(modelIDs, snapshot.FeaturedModelIDs()), snapshot
}
