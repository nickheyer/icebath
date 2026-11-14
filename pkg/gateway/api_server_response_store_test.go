package gateway

import (
	"context"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestReopenPrunesSettledIdentitiesToTheBound(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "response_store.db")
	store, err := NewResponseStore(ctx, 10, path, 10)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		key := "idem:scope:k" + strconv.Itoa(index)
		bound, err := store.BindRequestKey(ctx, key, "fp", nil)
		if err != nil || !bound {
			t.Fatalf("bind %s: %v %v", key, bound, err)
		}
		if err := store.Put(ctx, key, pyjson.ObjectOf("fingerprint", "fp", "response", pyjson.ObjectOf("id", "resp_"+strconv.Itoa(index)))); err != nil {
			t.Fatal(err)
		}
	}
	var settled int
	if err := store.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM response_keys WHERE settled_at IS NOT NULL").Scan(&settled); err != nil || settled != 4 {
		t.Fatalf("settled %d %v", settled, err)
	}
	store.Close(ctx)
	reopened, err := NewResponseStore(ctx, 10, path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(ctx)
	rows, err := reopened.conn.QueryContext(ctx, "SELECT request_key FROM response_keys")
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for rows.Next() {
		var key string
		rows.Scan(&key)
		kept = append(kept, key)
	}
	rows.Close()
	if !reflect.DeepEqual(kept, []string{"idem:scope:k3"}) {
		t.Fatalf("kept %v", kept)
	}
}
