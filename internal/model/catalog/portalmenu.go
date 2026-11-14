package catalog

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
)

type PortalModelMenu struct {
	Models             []string
	Pricing            *PriceTable
	Unavailable        []string
	UnavailableMessage string
	PolicyNotice       string
	FreeTier           bool
	Lines              []string
	Usage              PlanSnapshot
}

type PortalModelPrompt struct {
	Menu            PortalModelMenu
	PortalURL       string
	ConfirmProvider string
	ConfirmBaseURL  string
	ConfirmKey      auth.Secret
}

type PortalModelPrompter interface {
	Notice(ctx context.Context, lines []string) error
	Select(ctx context.Context, p PortalModelPrompt) (string, error)
}

type PortalSetup struct {
	catalog  *Catalog
	prompter PortalModelPrompter
	saved    func(ctx context.Context, model string)
}

var ErrPortalSetup = errors.New("catalog: the portal model setup needs a prompter and a saved-choice observer")

var _ portal.ModelSetup = (*PortalSetup)(nil)

func (c *Catalog) PortalSetup(prompter PortalModelPrompter, saved func(ctx context.Context, model string)) (*PortalSetup, error) {
	if prompter == nil || saved == nil {
		return nil, ErrPortalSetup
	}
	return &PortalSetup{catalog: c, prompter: prompter, saved: saved}, nil
}

func (s *PortalSetup) Pick(ctx context.Context, r portal.PickRequest) (string, error) {
	c := s.catalog
	ctx = c.scoped(ctx)
	menu := c.portalModelMenu(ctx, r.PortalURL)
	lines := []string{""}
	if len(menu.Models) > 0 && menu.PolicyNotice != "" {
		lines = append(lines, menu.PolicyNotice)
	}
	lines = append(lines, menu.Lines...)
	if err := s.prompter.Notice(ctx, lines); err != nil {
		return "", err
	}
	if len(menu.Models) == 0 {
		return "", nil
	}
	return s.prompter.Select(ctx, PortalModelPrompt{
		Menu: menu, PortalURL: r.PortalURL, ConfirmProvider: providers.ProviderPortal,
		ConfirmBaseURL: r.InferenceBaseURL, ConfirmKey: r.RuntimeKey,
	})
}

func (s *PortalSetup) Save(ctx context.Context, model string) error {
	c := s.catalog
	ctx = c.scoped(ctx)
	locked, unlock := c.cfg.Lock(ctx)
	defer unlock()
	cfg, err := c.cfg.Load(locked)
	if err != nil {
		return err
	}
	if cfg.Model.Bare {
		cfg.Model = config.ModelSetting{}
	}
	cfg.Model.Default = config.ModelRef{ID: model}
	if err := c.cfg.Save(locked, cfg); err != nil {
		return err
	}
	s.saved(ctx, model)
	return nil
}

func (s *PortalSetup) LargeModelNotice(ctx context.Context, model string) string {
	return s.catalog.plan.LargeModelNotice(s.catalog.scoped(ctx), model, providers.ProviderPortal)
}

func (c *Catalog) PortalModelMenu(ctx context.Context, portalURL string) PortalModelMenu {
	return c.portalModelMenu(c.scoped(ctx), portalURL)
}

func (c *Catalog) portalModelMenu(ctx context.Context, portalURL string) PortalModelMenu {
	menu := PortalModelMenu{Models: c.curatedPortalModelIDs(ctx), Pricing: newPriceTable(), Unavailable: []string{}}
	narrowed := false
	if len(menu.Models) > 0 {
		menu.Pricing = c.pricingForProvider(ctx, providers.ProviderPortal, "", false, false)
		menu.FreeTier = c.checkPortalFreeTier(ctx, true, false)
		allowed := c.portalPolicyAllowedIDs(ctx, false)
		tierKey := "paidRecommendedModels"
		if menu.FreeTier {
			menu.UnavailableMessage = c.portal.PortalEntitlementMessage(ctx, "paid Icebath Portal models")
			tierKey = "freeRecommendedModels"
		}
		menu.Models, menu.Pricing = c.unionWithPortalRecommendations(ctx, tierKey, menu.Models, menu.Pricing, portalURL, false, menu.FreeTier)
		if !menu.FreeTier {
			menu.Models = UnionWithPortalOnSaleModels(menu.Models, menu.Pricing)
		}
		before := menu.Models
		menu.Models = RestrictToPortalPolicy(menu.Models, allowed, true)
		narrowed = !slices.Equal(menu.Models, before)
		if menu.FreeTier {
			menu.Models, menu.Unavailable = PartitionPortalModelsByTier(menu.Models, menu.Pricing, true)
		}
	}
	switch {
	case len(menu.Models) > 0:
		menu.PolicyNotice = c.portal.PolicyNotice(ctx, narrowed)
		menu.Models, menu.Usage = c.featuredFirst(ctx, menu.Models, PlanWarm)
		menu.Lines = []string{"Showing " + strconv.Itoa(len(menu.Models)) + " curated models — use \"Enter custom model name\" for others."}
	case len(menu.Unavailable) > 0:
		upgrade := menu.UnavailableMessage
		if upgrade == "" {
			upgrade = "Upgrade at " + strings.TrimRight(firstNonEmpty(portalURL, auth.DefaultPortalURL), "/") + " to access paid models."
		}
		menu.Lines = []string{"No free models currently available.", upgrade}
	default:
		menu.Lines = []string{"No curated models available for Icebath Portal."}
	}
	return menu
}
