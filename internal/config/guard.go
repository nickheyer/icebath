package config

import (
	"errors"
	"strings"
)

// Owner of the keys under a prefix, which every write a person asks for of them goes through, such as a plugin whose settings name things
// it keeps or go together. Each is handed the full key as the settings keep it, the parts icebath declares spelled as declared and the names
// a person gives as they were written, and the value read, nil putting the default back
type Guard struct {
	// Why a value kept under the key can't be used, nil when it can: what a write of it would refuse. Nil takes every value
	Check func(cfg *Config, key string, value any) error
	// Checks a write as Check does and makes it itself, true when it did; false leaves it to the generic one, which a refusal stops. Nil
	// leaves every write Check takes to the generic one
	Write func(cfg *Config, key string, value any) (bool, error)
}

// Hands the writes a person asks for of the key and every key under it to the guard, the guard of the longest prefix taking a key. The
// prefix is a key icebath declares, which a key answers to in any case
func (c *Config) Guard(prefix string, guard Guard) {
	c.guardMu.Lock()
	defer c.guardMu.Unlock()
	if c.guards == nil {
		c.guards = map[string]Guard{}
	}
	c.guards[strings.TrimSpace(prefix)] = guard
}

// Guard of the key, false when none owns it
func (c *Config) guard(key string) (Guard, bool) {
	c.guardMu.RLock()
	defer c.guardMu.RUnlock()
	var longest string
	var owner Guard
	found := false
	for prefix, guard := range c.guards {
		if (same(prefix, key, spelled) || below(key, prefix, spelled)) && len(prefix) > len(longest) {
			longest, owner, found = prefix, guard, true
		}
	}
	return owner, found
}

// Why the owner of the key refuses the value, nil when it takes it or no guard owns the key
func (c *Config) Refused(key string, value any) error {
	key = c.Key(key)
	guard, ok := c.guard(key)
	if !ok || guard.Check == nil {
		return nil
	}
	return guard.Check(c, key, value)
}

// Saves the value as everything kept under the prefix, in one write, so that no reader sees half of a change of several keys under it; a
// view made Within writes it only while the prefix is kept
func (v View) Replace(value any) error {
	if v.config == nil || v.bare {
		return errors.New("config: nothing to keep under " + v.prefix)
	}
	return v.config.write(v.within, v.prefix, value)
}

// Saves a value a person asked for under the key, nil removing it: through the guard that owns the key, which checks it and may make the
// write itself, else as Set does
func (c *Config) Change(key string, value any) error {
	key = c.Key(key)
	guard, ok := c.guard(key)
	if !ok {
		return c.Set(key, value)
	}
	switch {
	case guard.Write != nil:
		if done, err := guard.Write(c, key, value); done || err != nil {
			return err
		}
	case guard.Check != nil:
		if err := guard.Check(c, key, value); err != nil {
			return err
		}
	}
	return c.Set(key, value)
}
