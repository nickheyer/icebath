package tools

import (
	"context"
	"errors"
	"testing"
)

func TestConnectorAccountOperationReusesTheOpenOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	LiveResetForTests()
	t.Cleanup(LiveResetForTests)
	ctx := context.Background()
	alias := "work"
	target := NewConnTarget("gmail", "connector", "connect")
	target.Alias = &alias
	op := NewConnectionOperation([]*ConnTarget{target}, "account:x", "", nil)
	if err := LiveOpen(ctx, op); err != nil {
		t.Fatal(err)
	}

	var open *OperationAlreadyOpen
	err := LiveOpen(ctx, NewConnectionOperation(nil, "account:x", "", nil))
	if !errors.As(err, &open) || open.Existing != op || err.Error() != "session 'account:x' already has operation "+op.OpID+" open" {
		t.Fatalf("second open of one session %v", err)
	}

	start, err := FindOrStartAccountOperation(ctx, &Env{Home: home}, []string{"gmail"}, "connect", "", &alias, nil, "")
	if err != nil || start.Started || start.Operation != op {
		t.Fatalf("found start %#v %v", start, err)
	}
	select {
	case <-start.done:
		t.Fatal("a found operation never runs a prepare step, so its prepare wait must not be signalled")
	default:
	}

	var mismatch *AccountOperationMismatch
	if _, err := FindOrStartAccountOperation(ctx, &Env{Home: home}, []string{"gmail", "slack"}, "connect", "", &alias, nil, ""); !errors.As(err, &mismatch) {
		t.Fatalf("connectors outside the open operation %v", err)
	}
}
