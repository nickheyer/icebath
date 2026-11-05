package skills

import (
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Write origin the self improvement review fork binds before its tool loop
const BackgroundReview = "background_review"

// Env value keys carrying the write origin, the attended flag and the review read marks
const (
	writeOriginKey      = "write_origin"
	reviewAttendedKey   = "review_attended"
	reviewReadMarksKey  = "background_review_read_paths"
	foregroundOrigin    = "foreground"
	writeOriginFallback = foregroundOrigin
)

// Binds the skill write origin on a session env, foreground when origin is empty
func SetCurrentWriteOrigin(env *tools.Env, origin string) {
	if env == nil || env.Values == nil {
		return
	}
	if origin == "" {
		origin = foregroundOrigin
	}
	env.Values.Store(writeOriginKey, origin)
}

// Write origin bound on the env, foreground for every regular agent
func CurrentWriteOrigin(env *tools.Env) string {
	if env == nil || env.Values == nil {
		return writeOriginFallback
	}
	if v, ok := env.Values.Load(writeOriginKey); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return writeOriginFallback
}

// Reports whether the env runs inside the background review fork
func IsBackgroundReview(env *tools.Env) bool { return CurrentWriteOrigin(env) == BackgroundReview }

// Marks whether a user attends the background review
func SetReviewAttended(env *tools.Env, attended bool) {
	if env != nil && env.Values != nil {
		env.Values.Store(reviewAttendedKey, attended)
	}
}

// Reports a background review nobody attends
func IsUnattendedReview(env *tools.Env) bool {
	if !IsBackgroundReview(env) {
		return false
	}
	if env.Values != nil {
		if v, ok := env.Values.Load(reviewAttendedKey); ok {
			if b, ok := v.(bool); ok && b {
				return false
			}
		}
	}
	return true
}

// Resolved skill file paths the review fork read within one run
type reviewReadMarks struct {
	mu    sync.Mutex
	paths map[string]bool
}

// Resolved path text, the lexical path when resolution fails
func resolvedStr(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		if abs, err := filepath.Abs(r); err == nil {
			return abs
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// Records a skill file the active background review fork read so later writes pass the guard
func markBackgroundReviewSkillRead(env *tools.Env, path string) {
	if !IsBackgroundReview(env) || env.Values == nil {
		return
	}
	v, _ := env.Values.LoadOrStore(reviewReadMarksKey, &reviewReadMarks{paths: map[string]bool{}})
	marks := v.(*reviewReadMarks)
	marks.mu.Lock()
	marks.paths[resolvedStr(path)] = true
	marks.mu.Unlock()
}

// Reports whether the review fork read the skill file in this run
func backgroundReviewHasRead(env *tools.Env, path string) bool {
	if env == nil || env.Values == nil {
		return false
	}
	v, ok := env.Values.Load(reviewReadMarksKey)
	if !ok {
		return false
	}
	marks := v.(*reviewReadMarks)
	marks.mu.Lock()
	defer marks.mu.Unlock()
	return marks.paths[resolvedStr(path)]
}

func init() {
	tools.MarkBackgroundReviewSkillRead = func(env *tools.Env, resolved string) { markBackgroundReviewSkillRead(env, resolved) }
}
