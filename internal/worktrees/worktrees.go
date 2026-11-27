// Package worktrees is the git worktrees sessions work in apart from the user's checkout: made under .worktrees of a repository on a
// branch of their own, locked for the process or the session using them, archived to a ref before an idle one holding work is removed,
// restored from that ref, and reclaimed along with the branches merged upstream. Features that work in trees add to their upkeep through
// hooks: why they still need a tree, what they let go of before one is removed, the repositories they keep and what they reclaim after
package worktrees

import (
	"context"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/sessions"
	worktreesconfig "github.com/nickheyer/icebath/internal/worktrees/config"
)

// What a feature that works in trees adds to their upkeep, each part left nil when it has none
type Hooks struct {
	// Why the feature still needs the tree, such as a scheduled job that runs in it; empty when it doesn't
	Holds func(ctx context.Context, tree string) string
	// Lets go of what the feature keeps rooted in the tree, such as language servers, before the tree is removed
	Release func(tree string)
	// Hears of a repository a tree was made or restored in, such as to keep its trees pruned on a schedule
	Recorded func(ctx context.Context, repo string)
	// Reclaims what the feature keeps for checkouts that are gone, once trees were pruned
	Reclaim func(ctx context.Context)
}

type Registrant interface {
	Hooks() Hooks
}

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in
	host *Worktrees
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

// Host the registry is nested in, through which plugins make, archive and reclaim trees
func (r *Registry) Host() *Worktrees {
	return r.host
}

// Hooks of every registrant, run in turn
func (r *Registry) Hooks() Hooks {
	return Hooks{
		Holds: func(ctx context.Context, tree string) string {
			for _, registrant := range r.registrants {
				if holds := registrant.Hooks().Holds; holds != nil {
					if why := holds(ctx, tree); why != "" {
						return why
					}
				}
			}
			return ""
		},
		Release: func(tree string) {
			for _, registrant := range r.registrants {
				if release := registrant.Hooks().Release; release != nil {
					release(tree)
				}
			}
		},
		Recorded: func(ctx context.Context, repo string) {
			for _, registrant := range r.registrants {
				if recorded := registrant.Hooks().Recorded; recorded != nil {
					recorded(ctx, repo)
				}
			}
		},
		Reclaim: func(ctx context.Context) {
			for _, registrant := range r.registrants {
				if reclaim := registrant.Hooks().Reclaim; reclaim != nil {
					reclaim(ctx)
				}
			}
		},
	}
}

type Worktrees struct {
	Registry
	config *config.Config
	// Store whose sessions say whether a tree is still worked in
	sessions *sessions.Sessions
}

func New(cfg *config.Config, stores *sessions.Sessions) *Worktrees {
	worktreesconfig.Register(cfg)
	return &Worktrees{config: cfg, sessions: stores}
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (w *Worktrees) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = w
	}
	w.Registry.Register(registrant)
}

// Settings of the profile the archive and the sweep follow
type Settings struct {
	// Idle trees holding work are archived and removed
	ArchiveDirty bool
	// Largest untracked file in MB an archive takes, any for zero
	ArchiveMaxFileMB float64
	// Days an archive is kept, for good for zero
	ArchiveRetentionDays float64
	// GB every tree together may take, any for zero
	MaxTotalGB float64
}

// Settings of the profile, none below zero
func (w *Worktrees) Settings(cfg *config.Config) Settings {
	return Settings{
		ArchiveDirty:         cfg.GetBool(worktreesconfig.ArchiveDirty),
		ArchiveMaxFileMB:     float64(max(cfg.GetInt(worktreesconfig.ArchiveMaxFile), 0)),
		ArchiveRetentionDays: float64(max(cfg.GetInt(worktreesconfig.ArchiveRetention), 0)),
		MaxTotalGB:           float64(max(cfg.GetInt(worktreesconfig.MaxTotal), 0)),
	}
}

// Whether the sweep acts as the pruner before archives: nothing is archived and no size is kept
func (s Settings) Classic() bool { return !s.ArchiveDirty && s.MaxTotalGB == 0 }

// Folder of the profile, where its record of trees, its cache of merge verdicts and its archives of untracked files are kept
func profileDir(cfg *config.Config) string {
	return cfg.ProfileDir(cfg.Profile())
}
