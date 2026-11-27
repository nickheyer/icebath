package launch

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"

	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/launch"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/worktrees"
	worktreesconfig "github.com/nickheyer/icebath/internal/worktrees/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	worktreesv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/worktrees/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/worktrees/v1/worktreesv1connect"
)

// Failure of a launch with the exit status it ends with, its lines already printed
type exit int

func (e exit) Error() string { return "" }

func (e exit) ExitCode() int { return int(e) }

// Trees this process works in: the one its launch made, and those /worktree new made, the newest the active one. Each is settled once the
// app ends
type Trees struct {
	mu      sync.Mutex
	launch  *worktreesv1.Tree
	created []*worktreesv1.Tree
}

// Notes a tree /worktree new made, the one the window works in from now on
func (t *Trees) Made(tree *worktreesv1.Tree) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.created = append(t.created, tree)
}

// Tree the window works in: the newest /worktree new made, else the launch's own; nil for none
func (t *Trees) Active() *worktreesv1.Tree {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.created) > 0 {
		return t.created[len(t.created)-1]
	}
	return t.launch
}

// Makes the launch's own tree before the app runs, brings back the removed tree of a session resumed, and settles every tree the run
// worked in once it ends
type launcher struct {
	trees *Trees
}

func Register(registry *launch.Registry, trees *Trees) {
	registry.Register(&launcher{trees: trees})
}

func (l *launcher) Flags() []launch.Flag {
	return []launch.Flag{{Names: []string{"-w", "--worktree"}, Help: "Run the session in its own git worktree"}}
}

// Whether the profile in use starts every launch in a tree of its own
func own(ctx context.Context, conn *connect.Client) bool {
	setting, err := configv1connect.NewConfigServiceClient(conn).Get(ctx, &configv1.GetRequest{Key: worktreesconfig.Own})
	on := false
	if err == nil {
		yaml.Unmarshal([]byte(setting.GetSetting().GetValue()), &on)
	}
	return on
}

func say(lines []string) {
	for _, line := range lines {
		fmt.Println(line)
	}
}

func (l *launcher) Around(ctx context.Context, conn *connect.Client, given launch.Given, run launch.Run) error {
	service := worktreesv1connect.NewWorktreeServiceClient(conn)
	want := given.Has("-w") || own(ctx, conn)
	start, err := os.Getwd()
	if err != nil {
		return err
	}
	if id := sessions.Resumed(ctx); id != "" && !want {
		if recreated, err := service.Recreate(ctx, &worktreesv1.RecreateRequest{SessionId: id}); err == nil && recreated.GetPath() != "" {
			fmt.Println("↪ recreated the session's worktree: " + recreated.GetPath())
			if err := os.Chdir(recreated.GetPath()); err != nil {
				fmt.Printf("⚠ Could not enter the session's worktree %s: %v\n", recreated.GetPath(), err)
			}
		}
	}
	if want {
		created, err := service.Create(ctx, &worktreesv1.CreateRequest{Directory: start, Pid: int64(os.Getpid()), Launch: true})
		if err != nil {
			fmt.Fprintln(os.Stderr, "✗ Failed to create the worktree: "+err.Error())
			return exit(1)
		}
		say(created.GetLines())
		if created.GetTree() == nil {
			return exit(1)
		}
		if err := os.Chdir(created.GetTree().GetPath()); err != nil {
			return err
		}
		l.trees.mu.Lock()
		l.trees.launch = created.GetTree()
		l.trees.mu.Unlock()
	}
	err = run(ctx)
	ctx = context.WithoutCancel(ctx)
	os.Chdir(start)
	l.settleMade(ctx, service)
	l.trees.mu.Lock()
	tree := l.trees.launch
	l.trees.launch = nil
	l.trees.mu.Unlock()
	if tree != nil {
		choice := ""
		if err == nil {
			choice = ask(ctx, service, tree)
		}
		if settled, settleErr := service.Settle(ctx, &worktreesv1.SettleRequest{Tree: tree, Choice: choice, Launch: true, Pid: int64(os.Getpid())}); settleErr == nil {
			say(settled.GetLines())
		} else {
			fmt.Fprintln(os.Stderr, "Could not settle the worktree "+tree.GetPath()+": "+settleErr.Error())
		}
	}
	return err
}

// Settles the trees /worktree new made, newest first, asking what to do with the one the window worked in last
func (l *launcher) settleMade(ctx context.Context, service worktreesv1connect.WorktreeServiceClient) {
	l.trees.mu.Lock()
	created := slices.Clone(l.trees.created)
	l.trees.created = nil
	l.trees.mu.Unlock()
	for i, tree := range slices.Backward(created) {
		choice := ""
		if i == len(created)-1 {
			choice = ask(ctx, service, tree)
		}
		if settled, err := service.Settle(ctx, &worktreesv1.SettleRequest{Tree: tree, Choice: choice, Pid: int64(os.Getpid())}); err == nil {
			say(settled.GetLines())
		} else {
			fmt.Fprintln(os.Stderr, "Could not settle the worktree "+tree.GetPath()+": "+err.Error())
		}
	}
}

// Asks on the terminal what to do with a tree holding uncommitted changes or unpushed commits, kept with no terminal, for a clean tree, a
// tree another process works in, an answer that is no choice, or the end of input
func ask(ctx context.Context, service worktreesv1connect.WorktreeServiceClient, tree *worktreesv1.Tree) string {
	c := cli.Terminal(ctx)
	if !c.Attended() {
		return worktrees.ExitKeep
	}
	state, err := service.State(ctx, &worktreesv1.StateRequest{Tree: tree, Pid: int64(os.Getpid())})
	if err != nil || state.GetState() == "" || state.GetInUse() {
		return worktrees.ExitKeep
	}
	question := "This worktree has uncommitted changes: " + tree.GetPath()
	if state.GetState() == "unpushed" {
		question = "This worktree has commits that are not pushed: " + tree.GetPath()
	}
	c.Blank()
	c.Say(question)
	c.Say("  1) Keep the worktree (default)")
	c.Say("  2) Archive and remove it (restorable)")
	c.Say("  3) Remove it and discard the changes")
	answer, _ := c.Answer("Choose 1-3 [1]: ")
	switch answer {
	case "2":
		return worktrees.ExitArchive
	case "3":
		return worktrees.ExitDiscard
	}
	return worktrees.ExitKeep
}
