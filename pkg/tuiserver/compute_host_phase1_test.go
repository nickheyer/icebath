package tuiserver

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestAppendLogRecordSingleWriteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := appendLogRecord(path, fmt.Sprintf("line-%03d-", i)+strings.Repeat("x", 2000)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 32 {
		t.Fatalf("%d lines", len(lines))
	}
	var ids, want []string
	for i, line := range lines {
		ids = append(ids, strings.SplitN(line, "-", 3)[1])
		want = append(want, fmt.Sprintf("%03d", i))
		if !strings.HasSuffix(line, strings.Repeat("x", 2000)) {
			t.Fatalf("line %q torn", line[:min(len(line), 40)])
		}
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids %v", ids)
	}
}
