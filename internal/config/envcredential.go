package config

import (
	"context"
	"errors"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Raised for a credential save in a program that wired no credential stores to follow it
var ErrNoCredentialStores = errors.New("no credential stores are installed to follow a saved provider credential")

// Replaces or removes the credential fields of one config section that hold old, recording each dotted path touched
func fixCredentialFields(section *yaml.Node, path, old string, replacement *string, fields []string, touched *[]string) {
	if !isMapping(section) {
		return
	}
	for _, field := range fields {
		at := mappingIndex(section, field)
		if at < 0 || !isStringNode(section.Content[at]) || section.Content[at].Value != old {
			continue
		}
		if replacement != nil && *replacement != "" {
			section.Content[at] = stringNode(*replacement)
		} else {
			removeSlot(pathHop{section, at})
		}
		*touched = append(*touched, path+"."+field)
	}
}

// Rewrites or removes the inline credentials of the user config that hold old, returning the dotted paths touched
func (e *Env) ScrubConfigMirrors(ctx context.Context, old string, replacement *string) ([]string, error) {
	if old == "" {
		return nil, nil
	}
	s := e.s
	_, path, err := s.configPath(ctx)
	if err != nil {
		return nil, err
	}
	b, h, err := s.route(ctx, path)
	if err != nil {
		return nil, err
	}
	if exists, err := b.Exists(ctx, h); err != nil || !exists {
		return nil, err
	}
	locked, release, err := s.lockWrite(ctx, path)
	if err != nil {
		return nil, err
	}
	doc, err := s.ReadDocument(locked)
	if err != nil || len(doc.root.Content) == 0 {
		return nil, release()
	}
	root := plainCopy(doc.root)
	var touched []string
	keyed := []string{"api_key", "api"}
	fixCredentialFields(childOf(root, "model"), "model", old, replacement, keyed, &touched)
	if aux := childOf(root, "auxiliary"); isMapping(aux) {
		for i := 0; i+1 < len(aux.Content); i += 2 {
			fixCredentialFields(aux.Content[i+1], "auxiliary."+aux.Content[i].Value, old, replacement, keyed, &touched)
		}
	}
	switch custom := childOf(root, "custom_providers"); {
	case isMapping(custom):
		for i := 0; i+1 < len(custom.Content); i += 2 {
			fixCredentialFields(custom.Content[i+1], "custom_providers."+custom.Content[i].Value, old, replacement, keyed, &touched)
		}
	case isSequence(custom):
		for i, entry := range custom.Content {
			fixCredentialFields(entry, "custom_providers."+strconv.Itoa(i), old, replacement, keyed, &touched)
		}
	}
	if providers := childOf(root, "providers"); isMapping(providers) {
		for i := 0; i+1 < len(providers.Content); i += 2 {
			fixCredentialFields(providers.Content[i+1], "providers."+providers.Content[i].Value, old, replacement, []string{"api_key"}, &touched)
		}
	}
	if len(touched) == 0 {
		return nil, release()
	}
	return touched, errors.Join(s.writeState(locked, path, root, true, ""), release())
}

// Saves a provider credential in the home env file and reconciles its config mirrors and the stores that follow it
func (e *Env) SaveCredential(ctx context.Context, name, value string) ([]string, error) {
	if e.s.envs.credentialSaved == nil {
		return nil, ErrNoCredentialStores
	}
	if err := e.RequireWritable(ctx, name, "set"); err != nil {
		return nil, err
	}
	file, err := e.File(ctx)
	if err != nil {
		return nil, err
	}
	old := file[name]
	if err := e.Set(ctx, name, value); err != nil {
		return nil, err
	}
	var updated []string
	if value != "" && old != "" && old != value {
		if updated, err = e.ScrubConfigMirrors(ctx, old, &value); err != nil {
			return nil, err
		}
	}
	e.s.envs.credentialSaved(ctx, name)
	return updated, nil
}

// What removing a provider credential took out of each store, Found false when no store held it
type CredentialRemoval struct {
	Removed        bool
	PoolPruned     []string
	Providers      []string
	ConfigScrubbed []string
	Found          bool
}

// Removes a provider credential from the home env file, the stores that follow it and its config mirrors
func (e *Env) RemoveCredential(ctx context.Context, name string) (CredentialRemoval, error) {
	if e.s.envs.credentialRemoved == nil {
		return CredentialRemoval{}, ErrNoCredentialStores
	}
	if err := e.RequireWritable(ctx, name, "remove"); err != nil {
		return CredentialRemoval{}, err
	}
	file, err := e.File(ctx)
	if err != nil {
		return CredentialRemoval{}, err
	}
	old := file[name]
	var out CredentialRemoval
	if out.Removed, err = e.Remove(ctx, name); err != nil {
		return CredentialRemoval{}, err
	}
	if out.PoolPruned, out.Providers, err = e.s.envs.credentialRemoved(ctx, name); err != nil {
		return CredentialRemoval{}, err
	}
	if old != "" {
		if out.ConfigScrubbed, err = e.ScrubConfigMirrors(ctx, old, nil); err != nil {
			return CredentialRemoval{}, err
		}
	}
	out.Found = out.Removed || len(out.PoolPruned) > 0 || len(out.ConfigScrubbed) > 0
	return out, nil
}
