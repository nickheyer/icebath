package vault

import (
	"context"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"
)

func (s *secretService) searchAll(ctx context.Context, attributes map[string]string) ([]dbus.ObjectPath, []dbus.ObjectPath, error) {
	var unlocked, locked []dbus.ObjectPath
	if err := s.call(ctx, ssPath, ssService+".SearchItems", attributes).Store(&unlocked, &locked); err != nil {
		return nil, nil, ssError(err)
	}
	return unlocked, locked, nil
}

func (k *Keychain) lookupAttributes(ctx context.Context, attributes map[string]string) (string, error) {
	ss, err := dialSecretService(ctx)
	if err != nil {
		return "", err
	}
	defer ss.close()
	unlocked, locked, err := ss.searchAll(ctx, attributes)
	if err != nil {
		return "", err
	}
	var item dbus.ObjectPath
	switch {
	case len(unlocked) > 0:
		item = unlocked[0]
	case len(locked) > 0:
		item = locked[0]
		if err := ss.unlock(ctx, "the Secret Service item unlock prompt", item); err != nil {
			return "", err
		}
		still, err := ss.locked(ctx, item, ssItem)
		if err != nil {
			return "", err
		}
		if still {
			return "", keychainError(ErrDenied, "Failed to unlock the item!")
		}
	default:
		return "", keychainError(ErrNotFound, "no Secret Service item carries the requested attributes")
	}
	if err := ss.openSession(ctx); err != nil {
		return "", err
	}
	var sec ssSecret
	if err := ss.call(ctx, item, ssItem+".GetSecret", ss.session).Store(&sec); err != nil {
		return "", ssError(err)
	}
	value, err := ss.decode(sec)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(value) {
		return "", keychainError(errSecretServiceReply, "the Secret Service item holds a secret that is not UTF-8 text")
	}
	return string(value), nil
}

func (k *Keychain) storeAttributes(ctx context.Context, label string, attributes map[string]string, secret string) error {
	ss, err := dialSecretService(ctx)
	if err != nil {
		return err
	}
	defer ss.close()
	collection, err := ss.defaultCollection(ctx)
	if err != nil {
		return err
	}
	if err := ss.openSession(ctx); err != nil {
		return err
	}
	sec, err := ss.encode([]byte(secret))
	if err != nil {
		return err
	}
	props := map[string]dbus.Variant{
		ssItem + ".Label":      dbus.MakeVariant(label),
		ssItem + ".Attributes": dbus.MakeVariant(attributes),
	}
	var item, prompt dbus.ObjectPath
	if err := ss.call(ctx, collection, ssCollection+".CreateItem", props, sec, true).Store(&item, &prompt); err != nil {
		return ssError(err)
	}
	if len(item) > 1 {
		return nil
	}
	dismissed, _, err := ss.prompt(ctx, prompt, "the Secret Service prompt storing the item")
	if err != nil {
		return err
	}
	if dismissed {
		return keychainError(ErrDenied, "Prompt dismissed.")
	}
	return nil
}

func (k *Keychain) clearAttributes(ctx context.Context, attributes map[string]string) error {
	ss, err := dialSecretService(ctx)
	if err != nil {
		return err
	}
	defer ss.close()
	unlocked, locked, err := ss.searchAll(ctx, attributes)
	if err != nil {
		return err
	}
	if len(locked) > 0 {
		if err := ss.unlock(ctx, "the Secret Service item unlock prompt", locked...); err != nil {
			return err
		}
	}
	for _, item := range append(unlocked, locked...) {
		if err := ss.deleteItem(ctx, item); err != nil {
			return err
		}
	}
	return nil
}
