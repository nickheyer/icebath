//go:build linux

package tray

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const statusNotifierWatcher = "org.kde.StatusNotifierWatcher"

func hostRegistered(v dbus.Variant) bool {
	b, ok := v.Value().(bool)
	return ok && b
}

// WatchLinuxTrayHost confirms a StatusNotifier host is registered and calls lost when the watcher or host goes away
func WatchLinuxTrayHost(lost func()) (func(), error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	var once sync.Once
	ready := false
	var mu sync.Mutex
	dispose := func() { once.Do(func() { conn.Close() }) }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	fail := func(e error) (func(), error) {
		dispose()
		return nil, e
	}
	for _, rule := range []string{
		"type='signal',sender='org.freedesktop.DBus',interface='org.freedesktop.DBus',member='NameOwnerChanged',arg0='" + statusNotifierWatcher + "'",
		"type='signal',interface='" + statusNotifierWatcher + "',member='StatusNotifierHostUnregistered'",
	} {
		if call := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.AddMatch", 0, rule); call.Err != nil {
			return fail(call.Err)
		}
	}
	var owner string
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, statusNotifierWatcher).Store(&owner); err != nil {
		return fail(err)
	}
	var value dbus.Variant
	obj := conn.Object(owner, "/StatusNotifierWatcher")
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, statusNotifierWatcher, "IsStatusNotifierHostRegistered").Store(&value); err != nil {
		return fail(err)
	}
	if !hostRegistered(value) {
		return fail(errors.New("No system tray host is available"))
	}
	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)
	mu.Lock()
	ready = true
	mu.Unlock()
	go func() {
		for sig := range signals {
			nameLost := sig.Sender == "org.freedesktop.DBus" && sig.Name == "org.freedesktop.DBus.NameOwnerChanged" &&
				len(sig.Body) >= 2 && sig.Body[0] == statusNotifierWatcher && sig.Body[1] == owner
			hostGone := sig.Sender == owner && sig.Path == "/StatusNotifierWatcher" && sig.Name == statusNotifierWatcher+".StatusNotifierHostUnregistered"
			if nameLost || hostGone {
				dispose()
				mu.Lock()
				r := ready
				mu.Unlock()
				if r {
					lost()
				}
				return
			}
		}
		mu.Lock()
		r := ready
		ready = false
		mu.Unlock()
		if r {
			lost()
		}
	}()
	return func() {
		mu.Lock()
		ready = false
		mu.Unlock()
		dispose()
	}, nil
}
