package skills

import (
	"context"
	"sync"
)

// Who changes skills, as the change log records it
const (
	ActorAgent   = "agent"
	ActorCurator = "curator"
	ActorUser    = "user"
)

type reviewKey struct{}

type actorKey struct{}

// The background review of skills a context runs: the skill files it read, which it may change
type review struct {
	mu   sync.Mutex
	read map[string]bool
}

// Context of a background review of skills, the curator's own pass: a delete archives the skill, a write needs the file read in the
// same review first, and only skills the curator manages may go
func Review(ctx context.Context) context.Context {
	return context.WithValue(ctx, reviewKey{}, &review{read: map[string]bool{}})
}

func reviewing(ctx context.Context) *review {
	r, _ := ctx.Value(reviewKey{}).(*review)
	return r
}

// Context whose skill changes the change log records as made by the actor
func As(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// Who the context's changes are made by: the one named, else the curator in a review, else the agent
func actor(ctx context.Context) string {
	if a, _ := ctx.Value(actorKey{}).(string); a == ActorAgent || a == ActorCurator || a == ActorUser {
		return a
	}
	if reviewing(ctx) != nil {
		return ActorCurator
	}
	return ActorAgent
}

// Records that the background review of the context read a skill file, so a write to it may follow; read_file marks the files it reads
func MarkRead(ctx context.Context, path string) {
	markRead(ctx, path)
}

func markRead(ctx context.Context, path string) {
	r := reviewing(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.read[resolve(path)] = true
}

// Whether the background review of the context read the file
func readInReview(ctx context.Context, path string) bool {
	r := reviewing(ctx)
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.read[resolve(path)]
}
