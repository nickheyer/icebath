package tools

import (
	"context"
	"testing"
)

// Check closure returning v, every call yielding a distinct closure of one literal
func constantCheck(v bool) func(*Env) bool {
	return func(*Env) bool { return v }
}

func TestCheckCacheKeysEveryClosureOfOneLiteralSeparately(t *testing.T) {
	InvalidateCheckCache()
	t.Cleanup(InvalidateCheckCache)
	ctx, env := context.Background(), &Env{Home: t.TempDir()}
	down, up := constantCheck(false), constantCheck(true)
	if Default.checkCached(ctx, env, down) || !Default.checkCached(ctx, env, up) {
		t.Fatal("one closure's verdict answered for its sibling")
	}
	if v, ok := CachedCheckResult(env, down); !ok || v {
		t.Fatalf("down cached %v %v", v, ok)
	}
	if v, ok := CachedCheckResult(env, up); !ok || !v {
		t.Fatalf("up cached %v %v", v, ok)
	}
	memo := checkMemo{}
	if Default.memoCheck(ctx, env, down, memo) || !Default.memoCheck(ctx, env, up, memo) {
		t.Fatal("the definition pass memo merged sibling closures")
	}
	uncached, cached := NoCacheCheck(constantCheck(true)), constantCheck(true)
	Default.checkCached(ctx, env, uncached)
	Default.checkCached(ctx, env, cached)
	if _, ok := CachedCheckResult(env, uncached); ok {
		t.Fatal("a never cached check was cached")
	}
	if _, ok := CachedCheckResult(env, cached); !ok {
		t.Fatal("marking one closure never cached marked its sibling too")
	}
}
