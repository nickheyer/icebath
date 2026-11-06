package pyast

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func depthKind(src string) string {
	_, err := Parse(src)
	var le *LimitError
	var se *SyntaxError
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &le) && le.Class == "MemoryError":
		return "mem"
	case errors.As(err, &se):
		return "syntax"
	}
	return err.Error()
}

// Nesting N of each recorded context gives the CPython result and N plus one gives its MemoryError
func TestParseDepthLimitsMatchCPython(t *testing.T) {
	var rows [][]any
	if err := json.Unmarshal(readGzip(t, "depth_limits.json.gz"), &rows); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var bad []string
	next := make(chan []any)
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range next {
				ctx, pre, leaf, n, base := r[0].(string), r[1].(string), r[2].(string), int(r[3].(float64)), r[4].(string)
				at := depthKind(strings.ReplaceAll(ctx, "§", strings.Repeat(pre, n)+leaf))
				over := depthKind(strings.ReplaceAll(ctx, "§", strings.Repeat(pre, n+1)+leaf))
				if at != base || over != "mem" {
					mu.Lock()
					bad = append(bad, fmt.Sprintf("%q %q %q n=%d: got %s and %s, want %s and mem", ctx, pre, leaf, n, at, over, base))
					mu.Unlock()
				}
			}
		}()
	}
	for _, r := range rows {
		next <- r
	}
	close(next)
	wg.Wait()
	for _, b := range bad {
		t.Error(b)
	}
	if len(rows) < 2000 {
		t.Fatalf("only %d recorded rows", len(rows))
	}
}
