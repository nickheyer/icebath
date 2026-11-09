package kanban

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Board whose database stays at the legacy root kanban.db
const DefaultBoard = "default"

type currentBoardOverrideKey struct{}
type pinFirstBoardResolutionKey struct{}

// Context resolving board paths env pin first, for machine flows that enumerate every board
func PinFirstBoardResolution(ctx context.Context) context.Context {
	return context.WithValue(ctx, pinFirstBoardResolutionKey{}, true)
}

// Context pinning the active board for calls made with it only
func ScopedCurrentBoard(ctx context.Context, slug string) context.Context {
	return context.WithValue(ctx, currentBoardOverrideKey{}, slug)
}

// Scoped board override of the context, empty when none
func currentBoardOverride(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(currentBoardOverrideKey{}).(string)
	return s
}

// Optional board argument naming a slug, nil meaning the caller expressed no board
func BoardArg(slug string) *string { return &slug }

var boardSlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9\-_]{0,63}$`)

// Lowercased stripped slug, empty for no slug and a ValueError when invalid
func normalizeBoardSlug(slug *string) (string, error) {
	if slug == nil {
		return "", nil
	}
	s := pystr.Lower(pystr.Strip(*slug))
	if s == "" {
		return "", nil
	}
	if !boardSlugRE.MatchString(s) {
		return "", &ValueError{fmt.Sprintf("invalid board slug %s: must be 1-64 chars, lowercase "+
			"alphanumerics / hyphens / underscores, not starting with '-' or '_'", pyrepr.StrRepr(*slug))}
	}
	return s, nil
}

// NormalizeBoardSlug exports normalizeBoardSlug
func NormalizeBoardSlug(slug *string) (string, error) { return normalizeBoardSlug(slug) }

// Normalized slug or the default board
func slugOrDefault(board *string) (string, error) {
	s, err := normalizeBoardSlug(board)
	if err != nil || s != "" {
		return s, err
	}
	return DefaultBoard, nil
}

// Normalized slug or a ValueError when it is empty
func requireSlug(slug string) (string, error) {
	normed, err := normalizeBoardSlug(&slug)
	if err != nil {
		return "", err
	}
	if normed == "" {
		return "", &ValueError{"board slug is required"}
	}
	return normed, nil
}

var kanbanHomeFn = func() string {
	if override := pystr.Strip(os.Getenv("ICEBATH_KANBAN_HOME")); override != "" {
		return pypath.PathStr(pypath.Expanduser(override))
	}
	return home.Root()
}

// ICEBATH_KANBAN_HOME else the default root, shared across profiles so dispatcher and workers meet
func KanbanHome() string { return kanbanHomeFn() }

func init() {
	tools.KanbanHomeResolver = func(*tools.Env) string { return KanbanHome() }
}

// Parent of the additional named boards under the kanban root
func BoardsRoot() string { return pypath.PathJoin(KanbanHome(), "kanban", "boards") }

// File holding the slug boards switch writes
func CurrentBoardPath() string { return pypath.PathJoin(KanbanHome(), "kanban", "current") }

// Live board slug of a candidate, empty when malformed, missing or archived
func existingBoard(candidate string) string {
	if candidate == "" {
		return ""
	}
	normed, err := normalizeBoardSlug(&candidate)
	if err != nil || normed == "" || !BoardExists(&normed) {
		return ""
	}
	meta, err := ReadBoardMetadata(&normed)
	if err != nil || pyrepr.Truthy(objGet(meta, "archived")) {
		return ""
	}
	return normed
}

// Active slug from the context override, ICEBATH_KANBAN_BOARD, the current file while that board exists, else default
func GetCurrentBoard(ctx context.Context) string {
	for _, candidate := range []string{pystr.Strip(currentBoardOverride(ctx)), pystr.Strip(os.Getenv("ICEBATH_KANBAN_BOARD"))} {
		if found := existingBoard(candidate); found != "" {
			return found
		}
	}
	if raw, err := os.ReadFile(CurrentBoardPath()); err == nil {
		if text, err := pystr.DecodeUTF8Sig(raw); err == nil {
			if val := pystr.Strip(text); val != "" {
				if normed, err := normalizeBoardSlug(&val); err == nil && normed != "" && BoardExists(&normed) {
					return normed
				}
			}
		}
	}
	return DefaultBoard
}

// Persists slug as the active board and returns the file written, leaving existence checks to callers
func SetCurrentBoard(ctx context.Context, slug string) (string, error) {
	if err := assertNotDelegatedChildMutation(ctx, ""); err != nil {
		return "", err
	}
	normed, err := requireSlug(slug)
	if err != nil {
		return "", err
	}
	path := CurrentBoardPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(normed+"\n"), 0o666)
}

// Removes the current file so the active board reverts to default
func ClearCurrentBoard(ctx context.Context) error {
	if err := assertNotDelegatedChildMutation(ctx, ""); err != nil {
		return err
	}
	if err := os.Remove(CurrentBoardPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Board directory under boards root, metadata only for default whose database stays at the root
func BoardDir(board *string) (string, error) {
	slug, err := slugOrDefault(board)
	if err != nil {
		return "", err
	}
	return pypath.PathJoin(BoardsRoot(), slug), nil
}

// Reports whether the board has a board.json on disk, default always existing
func BoardExists(board *string) bool {
	slug, err := slugOrDefault(board)
	if err != nil {
		return false
	}
	if slug == DefaultBoard {
		return true
	}
	return dirHoldsBoard(pypath.PathJoin(BoardsRoot(), slug))
}

// Explicit caller intent from the board argument else the scoped override, empty when neither
func explicitBoardSlug(ctx context.Context, board *string) (string, error) {
	if board != nil {
		return normalizeBoardSlug(board)
	}
	if c := pystr.Strip(currentBoardOverride(ctx)); c != "" {
		if normed, err := normalizeBoardSlug(&c); err == nil {
			return normed, nil
		}
	}
	return "", nil
}

// Reports whether explicit board intent must still resolve through the env pins
func explicitBoardIntentPinned(ctx context.Context) bool {
	if ctx != nil {
		if pinned, _ := ctx.Value(pinFirstBoardResolutionKey{}).(bool); pinned {
			return true
		}
	}
	return tools.ExplicitBoardIntentIsPinned(ctx)
}

// Shared resolver where explicit intent outranks the env pin only where no fence applies
func boardPath(ctx context.Context, envVar string, board *string, defaultParts []string, leaf string) (string, error) {
	pin := ""
	if envVar != "" {
		pin = pystr.Strip(os.Getenv(pyEnvName(envVar)))
	}
	slug, err := explicitBoardSlug(ctx, board)
	if err != nil {
		return "", err
	}
	if pin != "" && (slug == "" || explicitBoardIntentPinned(ctx)) {
		return pypath.PathStr(pypath.Expanduser(pin)), nil
	}
	if slug == "" {
		slug = GetCurrentBoard(ctx)
	}
	if slug == DefaultBoard {
		return pypath.PathJoin(KanbanHome(), defaultParts...), nil
	}
	return pypath.PathJoin(BoardsRoot(), slug, leaf), nil
}

var kanbanDBPathFn func(ctx context.Context, board *string) (string, error)

func init() {
	kanbanDBPathFn = func(ctx context.Context, board *string) (string, error) {
		return boardPath(ctx, "HERMES_KANBAN_DB", board, []string{"kanban.db"}, "kanban.db")
	}
}

// kanban.db path pinned by ICEBATH_KANBAN_DB, the root kanban.db for default, else the board dir
func KanbanDBPath(ctx context.Context, board *string) (string, error) {
	return kanbanDBPathFn(ctx, board)
}

// Per board scratch workspace root, ICEBATH_KANBAN_WORKSPACES_ROOT winning
func WorkspacesRoot(ctx context.Context, board *string) (string, error) {
	return boardPath(ctx, "HERMES_KANBAN_WORKSPACES_ROOT", board, []string{"kanban", "workspaces"}, "workspaces")
}

// Per board attachments root, ICEBATH_KANBAN_ATTACHMENTS_ROOT winning
func AttachmentsRoot(ctx context.Context, board *string) (string, error) {
	return boardPath(ctx, "HERMES_KANBAN_ATTACHMENTS_ROOT", board, []string{"kanban", "attachments"}, "attachments")
}

// Per task attachment directory under the attachments root
func TaskAttachmentsDir(ctx context.Context, taskID string, board *string) (string, error) {
	root, err := AttachmentsRoot(ctx, board)
	if err != nil {
		return "", err
	}
	return pypath.PathJoin(root, taskID), nil
}

// Per board worker log directory
func WorkerLogsDir(ctx context.Context, board *string) (string, error) {
	return boardPath(ctx, "", board, []string{"kanban", "logs"}, "logs")
}
