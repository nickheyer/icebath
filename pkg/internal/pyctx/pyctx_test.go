package pyctx

import (
	"context"
	"testing"
)

func TestVarSetGetResetFollowContextvars(t *testing.T) {
	v := NewVar("v", "default")
	c := New()
	ctx := With(context.Background(), c)
	if got := v.Get(ctx); got != "default" {
		t.Fatalf("default %v", got)
	}
	tok := v.Set(ctx, "one")
	tok2 := v.Set(ctx, "two")
	if v.Get(ctx) != "two" || len(c.Items()) != 1 {
		t.Fatalf("set %v %v", v.Get(ctx), c.Items())
	}
	if err := v.Reset(ctx, tok2); err != nil || v.Get(ctx) != "one" {
		t.Fatalf("reset %v %v", err, v.Get(ctx))
	}
	if err := v.Reset(ctx, tok2); err == nil || err.Error() != "Token has already been used once" {
		t.Fatalf("reuse %v", err)
	}
	if err := NewVar("w", nil).Reset(ctx, tok); err == nil || err.Error() != "Token was created by a different ContextVar" {
		t.Fatalf("foreign var %v", err)
	}
	if err := v.Reset(With(ctx, New()), tok); err == nil || err.Error() != "Token was created in a different Context" {
		t.Fatalf("foreign context %v", err)
	}
	if err := v.Reset(ctx, tok); err != nil || v.Get(ctx) != "default" || len(c.Items()) != 0 {
		t.Fatalf("unset %v %v %v", err, v.Get(ctx), c.Items())
	}
}

func TestCopyIsIndependentAndUnenteredCallersShareTheMainContext(t *testing.T) {
	v := NewVar("copy", 0)
	c := New()
	ctx := With(context.Background(), c)
	v.Set(ctx, 1)
	cp := CopyContext(ctx)
	v.Set(With(ctx, cp), 2)
	if v.Get(ctx) != 1 || v.Get(With(ctx, cp)) != 2 {
		t.Fatalf("copy shares values %v %v", v.Get(ctx), v.Get(With(ctx, cp)))
	}
	if Current(context.Background()) != Current(nil) || Current(ctx) != c {
		t.Fatal("main context differs between unentered callers")
	}
	items := cp.Items()
	if len(items) != 1 || items[0].Var != v || items[0].Value != 2 {
		t.Fatalf("items %v", items)
	}
}
