package gateway

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strconv"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Answers parent lookups from a table, a missing pid failing like ps for a dead process
func pinProcessParents(t *testing.T, parents map[int]string) *int {
	t.Helper()
	savedRun, savedOS := ancestorsRunPS, ancestorsGOOS
	t.Cleanup(func() { ancestorsRunPS, ancestorsGOOS = savedRun, savedOS })
	ancestorsGOOS = "linux"
	calls := 0
	ancestorsRunPS = func(ctx context.Context, argv []string) (*pysubprocess.Completed, error) {
		calls++
		pid, _ := strconv.Atoi(argv[len(argv)-1])
		out, ok := parents[pid]
		if !ok {
			return &pysubprocess.Completed{Returncode: 1}, nil
		}
		if out == "spawn-error" {
			return nil, errors.New("exec: ps not found")
		}
		return &pysubprocess.Completed{Returncode: 0, Stdout: []byte(out)}, nil
	}
	return &calls
}

func TestProcessAncestorsWalksNearestFirstAndStopsAtInitCyclesAndDepth(t *testing.T) {
	ctx := context.Background()
	pinProcessParents(t, map[int]string{50: "  40\n", 40: "30", 30: "1"})
	if got := ProcessAncestors(ctx, 50); !reflect.DeepEqual(got, []int{40, 30}) {
		t.Fatalf("chain %v", got)
	}
	pinProcessParents(t, map[int]string{50: "40", 40: "50"})
	if got := ProcessAncestors(ctx, 50); !reflect.DeepEqual(got, []int{40}) {
		t.Fatalf("chain through the starting pid %v", got)
	}
	pinProcessParents(t, map[int]string{50: "40", 40: "30", 30: "40"})
	if got := ProcessAncestors(ctx, 50); !reflect.DeepEqual(got, []int{40, 30}) {
		t.Fatalf("cyclic chain %v", got)
	}
	deep := map[int]string{}
	for pid := 100; pid > 80; pid-- {
		deep[pid] = strconv.Itoa(pid - 1)
	}
	pinProcessParents(t, deep)
	if got := ProcessAncestors(ctx, 100); len(got) != 8 || got[0] != 99 || got[7] != 92 {
		t.Fatalf("deep chain %v", got)
	}
	if got := processAncestors(ctx, 100, 2); !reflect.DeepEqual(got, []int{99, 98}) {
		t.Fatalf("depth two chain %v", got)
	}
	for _, out := range []string{"", "abc", "spawn-error", "   \n"} {
		pinProcessParents(t, map[int]string{50: out})
		if got := ProcessAncestors(ctx, 50); len(got) != 0 {
			t.Fatalf("lookup answering %q gave %v", out, got)
		}
	}
	calls := pinProcessParents(t, map[int]string{50: "40"})
	ancestorsGOOS = "windows"
	if got := ProcessAncestors(ctx, 50); len(got) != 0 || *calls != 0 {
		t.Fatalf("windows chain %v after %d lookups", got, *calls)
	}
}

func TestProcessPpidReadsTheRealParentOfThisProcess(t *testing.T) {
	if ancestorsGOOS == "windows" {
		if _, ok := processPpid(context.Background(), os.Getpid()); ok {
			t.Fatal("parent lookup answered on windows")
		}
		return
	}
	parent, ok := processPpid(context.Background(), os.Getpid())
	if !ok || parent != os.Getppid() {
		t.Fatalf("parent %d %v want %d", parent, ok, os.Getppid())
	}
}
