package scenario

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

var subcommandChoices = regexp.MustCompile(`(?m)^positional arguments:\n\s+\{([^}]+)\}`)

// Every subcommand hermes lists under a top level command exists and prints its own usage with exit 0
func TestScenarioHelpOfEverySubcommand(t *testing.T) {
	s := Start(t, Setup{Env: map[string]string{"COLUMNS": "100"}})
	fixtures, err := filepath.Glob(filepath.Join("testdata", "help", "*.txt"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no help fixtures under testdata/help: %v", err)
	}
	var calls [][]string
	for _, fixture := range fixtures {
		raw, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		choices := subcommandChoices.FindSubmatch(raw)
		if choices == nil {
			continue
		}
		command := strings.TrimSuffix(filepath.Base(fixture), ".txt")
		for _, sub := range strings.Split(strings.Join(strings.Fields(string(choices[1])), ""), ",") {
			calls = append(calls, []string{command, sub, "--help"})
		}
	}
	if len(calls) < 300 {
		t.Fatalf("the fixtures name %d subcommands, want the full command tree", len(calls))
	}
	queue := make(chan []string)
	var mu sync.Mutex
	var problems []string
	var workers sync.WaitGroup
	for w := 0; w < 6; w++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for args := range queue {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				out, err := s.command(ctx, Step{Args: args}, false).Output()
				cancel()
				text := string(out)
				switch {
				case err != nil:
					mu.Lock()
					problems = append(problems, strings.Join(args, " ")+": "+err.Error())
					mu.Unlock()
				case !strings.HasPrefix(text, "usage: icebath "+args[0]+" ") || !strings.Contains(text, "-h, --help"):
					mu.Lock()
					problems = append(problems, strings.Join(args, " ")+": no usage of its own, starts "+strings.SplitN(text, "\n", 2)[0])
					mu.Unlock()
				}
			}
		}()
	}
	for _, args := range calls {
		queue <- args
	}
	close(queue)
	workers.Wait()
	if len(problems) > 0 {
		sort.Strings(problems)
		s.Failf("%d of %d subcommands\n%s", len(problems), len(calls), strings.Join(problems, "\n"))
	}
}
