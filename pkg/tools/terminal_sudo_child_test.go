package tools

import (
	"sync"
	"testing"
)

func TestNoSudoUserForADelegateChildAndASingleQueryOnly(t *testing.T) {
	t.Setenv(delegatedChildEnvMarker, "")
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "")
	env := &Env{Values: &sync.Map{}}
	if noSudoUser(t.Context(), env) || noSudoUser(t.Context(), nil) {
		t.Fatal("an interactive parent counted as having no sudo user")
	}
	child := WithDelegatedChildContext(t.Context(), true)
	if !noSudoUser(child, env) || !noSudoUser(child, nil) {
		t.Fatal("a delegate child could still be asked for a sudo password")
	}
	t.Setenv(delegatedChildEnvMarker, "1")
	if noSudoUser(t.Context(), env) {
		t.Fatal("the subprocess marker alone silenced sudo prompting")
	}
	t.Setenv(delegatedChildEnvMarker, "")
	env.Values.Store("session.delegated_child", true)
	if !noSudoUser(t.Context(), env) {
		t.Fatal("the session child flag was not read")
	}
	env.Values.Store("session.delegated_child", false)
	if noSudoUser(t.Context(), env) {
		t.Fatal("a false session child flag silenced sudo prompting")
	}
	env.Values.Store("session.single_query", true)
	if !noSudoUser(t.Context(), env) {
		t.Fatal("a single query run could still be asked for a sudo password")
	}
}

func TestHandleSudoFailureAddsTheTipForADelegateChild(t *testing.T) {
	t.Setenv(delegatedChildEnvMarker, "")
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "")
	t.Setenv("ICEBATH_GATEWAY_SESSION", "")
	env := &Env{Values: &sync.Map{}}
	failure := "sudo: a password is required"
	if got := handleSudoFailure(t.Context(), env, failure, "local"); got != failure {
		t.Fatalf("an interactive parent got the headless tip %q", got)
	}
	got := handleSudoFailure(WithDelegatedChildContext(t.Context(), true), env, failure, "local")
	if got == failure {
		t.Fatal("a delegate child got no headless sudo tip")
	}
}
