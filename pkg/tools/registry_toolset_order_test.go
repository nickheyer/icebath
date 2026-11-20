package tools

import (
	"context"
	"reflect"
	"testing"
)

func TestAvailableToolsetNamesFirstRegistrationOrder(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry()
	r.Register(orderDef("t1", "beta"))
	r.Register(orderDef("t2", "alpha"))
	r.Register(orderDef("t3", "beta"))
	if got := r.AvailableToolsetNames(ctx, nil); !reflect.DeepEqual(got, []string{"beta", "alpha"}) {
		t.Fatalf("got %v", got)
	}
	r.Register(orderDef("t1", "beta"))
	if got := r.AvailableToolsetNames(ctx, nil); !reflect.DeepEqual(got, []string{"beta", "alpha"}) {
		t.Fatalf("replacement moved the key: %v", got)
	}
	r.Remove("t1")
	r.Remove("t3")
	r.Register(orderDef("t1", "beta"))
	if got := r.AvailableToolsetNames(ctx, nil); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("re-registration after removal must append: %v", got)
	}
	names := map[string]bool{}
	for n := range r.AvailableToolsets(ctx, nil) {
		names[n] = true
	}
	if len(names) != 2 || !names["alpha"] || !names["beta"] {
		t.Fatalf("AvailableToolsets keys %v", names)
	}
}

func TestAvailableToolsetNamesScopedOverlayAppendsInRegistrationOrder(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry()
	r.Register(orderDef("g1", "global_ts"))
	profile := t.TempDir()
	env := &Env{Home: profile}
	if err := r.RegisterScoped(profile, orderDef("p_zz", "zeta_ts")); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterScoped(profile, orderDef("p_aa", "alpha_ts")); err != nil {
		t.Fatal(err)
	}
	if got := r.AvailableToolsetNames(ctx, env); !reflect.DeepEqual(got, []string{"global_ts", "zeta_ts", "alpha_ts"}) {
		t.Fatalf("got %v", got)
	}
	if got := r.AvailableToolsetNames(ctx, nil); !reflect.DeepEqual(got, []string{"global_ts"}) {
		t.Fatalf("global view %v", got)
	}
}
