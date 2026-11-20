package commands

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Namespace a kanban invocation hands its handler
func kanbanNamespace(t *testing.T, argv ...string) map[string]any {
	t.Helper()
	c := NewCLI()
	var got map[string]any
	for _, path := range c.Unbound() {
		if path == "kanban" || strings.HasPrefix(path, "kanban ") {
			c.Bind(path, func(cmd *cobra.Command, args []string) error {
				got = c.Namespace(cmd, args)
				return nil
			})
		}
	}
	if err := c.Execute(append([]string{"kanban"}, argv...)); err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	if got == nil {
		t.Fatalf("%v: handler never ran", argv)
	}
	return got
}

func TestKanbanParserNamespaceDestsAndDefaults(t *testing.T) {
	cases := []struct {
		argv []string
		want map[string]any
	}{
		{[]string{"create", "t"}, map[string]any{"kanban_action": "create", "title": "t", "parent": []string{}, "skills": []string{},
			"model_override": nil, "provider_override": nil, "goal_mode": false, "goal_max_turns": nil, "max_retries": nil,
			"priority": 0, "created_by": "user", "initial_status": "running", "triage": false, "json": false}},
		{[]string{"create", "t", "--skill", "a", "--model", "m", "--provider", "p", "--goal", "--goal-max-turns", "3"},
			map[string]any{"skills": []string{"a"}, "model_override": "m", "provider_override": "p", "goal_mode": true, "goal_max_turns": 3}},
		{[]string{"swarm", "g", "--verifier", "v", "--synthesizer", "s"}, map[string]any{"kanban_action": "swarm", "goal": "g", "worker": []string{}, "created_by": nil}},
		{[]string{"ls", "--step-key", "k"}, map[string]any{"kanban_action": "ls", "current_step_key": "k", "status": nil, "sort": nil, "archived": false}},
		{[]string{"comment", "t1", "hi", "there"}, map[string]any{"text": []string{"hi", "there"}, "max_len": nil, "author": nil}},
		{[]string{"edit", "t1"}, map[string]any{"priority": nil, "title": nil, "result": nil}},
		{[]string{"block", "t1", "--ids", "t2", "t3"}, map[string]any{"task_id": "t1", "ids": []string{"t2", "t3"}, "reason": []string{}, "kind": nil}},
		{[]string{"archive", "--rm", "t1", "t2"}, map[string]any{"purge_ids": []string{"t1", "t2"}, "task_ids": []string{}}},
		{[]string{"dispatch"}, map[string]any{"max": nil, "failure_limit": 2, "dry_run": false}},
		{[]string{"daemon"}, map[string]any{"max": nil, "interval": 60.0, "force": false}},
		{[]string{"tail", "t1"}, map[string]any{"interval": 1.0}},
		{[]string{"watch"}, map[string]any{"interval": 0.5, "kinds": nil}},
		{[]string{"log", "t1"}, map[string]any{"tail": nil}},
		{[]string{"claim", "t1"}, map[string]any{"ttl": 900}},
		{[]string{"specify", "--all"}, map[string]any{"all_triage": true, "task_id": nil}},
		{[]string{"decompose", "t1"}, map[string]any{"all_triage": false, "task_id": "t1"}},
		{[]string{"gc", "--event-retention-days", "0"}, map[string]any{"event_retention_days": 0, "log_retention_days": 30}},
		{[]string{"attach-rm", "7"}, map[string]any{"attachment_id": 7}},
		{[]string{"boards", "new", "s"}, map[string]any{"kanban_action": "boards", "boards_action": "new", "slug": "s"}},
		{[]string{"boards", "import", "a.tgz", "--as", "x"}, map[string]any{"boards_action": "import", "archive": "a.tgz", "as_slug": "x"}},
		{[]string{"boards"}, map[string]any{"kanban_action": "boards", "boards_action": nil}},
		{[]string{"--board", "b", "stats"}, map[string]any{"kanban_action": "stats", "board": "b"}},
	}
	for _, tc := range cases {
		ns := kanbanNamespace(t, tc.argv...)
		for key, want := range tc.want {
			got, ok := ns[key]
			if !ok || !reflect.DeepEqual(got, want) {
				t.Errorf("%v: %s = %#v (present %v), want %#v", tc.argv, key, got, ok, want)
			}
		}
		if _, ok := ns["_kanban_parser"].(*cobra.Command); !ok {
			t.Errorf("%v: _kanban_parser missing", tc.argv)
		}
	}
}
