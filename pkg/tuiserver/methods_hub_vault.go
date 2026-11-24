//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
	"github.com/nickheyer/icebath/pkg/vault"
)

var hubVaultRegistry = &handlerRegistry{}

func init() {
	scopedRPC(hubVaultRegistry, tuigateway.VaultList.Name(), 5194, rpcShape{}, vaultList)
	scopedRPC(hubVaultRegistry, tuigateway.VaultChange.Name(), 5194, rpcShape{}, vaultChange)
	hubVaultRegistry.install()
}

// Answer for a failed vault request: a failure that is safe to show goes back as its message, anything else as the failure it is
func vaultFailed(rid any, err error) (map[string]any, error) {
	var shown *vault.VaultError
	if errors.As(err, &shown) {
		return rpcErr(rid, 4194, shown.Msg, nil), nil
	}
	return nil, err
}

// Everything the autofill vault screen shows
func vaultListing(ctx context.Context) (tuigateway.VaultListResult, error) {
	out := tuigateway.VaultListResult{Items: []tuigateway.VaultItemRow{}, Locked: []string{}, Sources: []tuigateway.VaultSourceRow{}, Kinds: []tuigateway.VaultKind{}}
	items, locked, err := browser.VaultItems(ctx)
	if err != nil {
		return out, err
	}
	for _, item := range items {
		out.Items = append(out.Items, tuigateway.VaultItemRow{Handle: item.Handle, Source: item.Source, Kind: item.Kind, Label: item.Label,
			Identifier: item.Identifier, Origin: item.Origin, Local: item.Local})
	}
	out.Locked = append(out.Locked, locked...)
	sources, err := browser.VaultSources(ctx)
	if err != nil {
		return out, err
	}
	for _, source := range sources {
		out.Sources = append(out.Sources, tuigateway.VaultSourceRow{Name: source.Name, DisplayName: source.DisplayName, State: source.State})
	}
	key, err := browser.VaultKey(ctx, home.Dir(ctx))
	if err != nil {
		return out, err
	}
	out.Key = tuigateway.VaultKeyInfo{Storage: key.Storage, Path: key.Path, Service: key.Service, Account: key.Account, Configured: key.Configured,
		Keychain: key.Keychain, KeychainLeft: key.KeychainLeft}
	for _, kind := range vault.VaultKinds {
		row := tuigateway.VaultKind{Kind: kind, Fields: []tuigateway.VaultField{}}
		for _, field := range browser.VaultKindFields(kind) {
			row.Fields = append(row.Fields, tuigateway.VaultField{Key: field.Key, Required: field.Required, Secret: field.Secret, Choices: field.Choices})
		}
		out.Kinds = append(out.Kinds, row)
	}
	return out, nil
}

// Items, password managers and key of the autofill vault for its screen
func vaultList(ctx context.Context, rid any, raw map[string]any, _ *tuihost.Session) (map[string]any, error) {
	if _, err := tuigateway.VaultList.Params(raw); err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	out, err := vaultListing(ctx)
	if err != nil {
		return vaultFailed(rid, err)
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// One change to the autofill vault, answered with the vault after it
func vaultChange(ctx context.Context, rid any, raw map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.VaultChange.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	switch p.Verb {
	case "add":
		_, err = browser.AddVaultItem(ctx, p.Kind, p.Label, p.Origin, p.Values)
	case "remove":
		var removed bool
		if removed, err = browser.RemoveVaultItem(ctx, p.Handle); err == nil && !removed {
			err = &vault.VaultError{Msg: "no vault item with handle " + p.Handle}
		}
	case "source":
		err = browser.SetVaultSource(ctx, home.Dir(ctx), p.Source, p.On)
	case "key":
		_, err = browser.MoveVaultKey(ctx, home.Dir(ctx), p.To)
	default:
		err = &vault.VaultError{Msg: "unknown vault change: " + p.Verb}
	}
	if err != nil {
		return vaultFailed(rid, err)
	}
	out, err := vaultListing(ctx)
	if err != nil {
		return vaultFailed(rid, err)
	}
	return ok(rid, tuigateway.Plain(out)), nil
}
