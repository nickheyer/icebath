//go:build !darwin && !windows

package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	notifyService         = "org.freedesktop.Notifications"
	notifyPath            = dbus.ObjectPath("/org/freedesktop/Notifications")
	dbusService           = "org.freedesktop.DBus"
	notifyDeliveryTimeout = 5 * time.Second
	notifyRetryCooldown   = 10 * time.Second
	notifyMaxPending      = 32
)

var actionKeyDigits = regexp.MustCompile(`^\d+$`)

type notifyReceiver struct {
	owner   string
	receive func(member string, value any)
}

type notifyConnection struct {
	conn       *dbus.Conn
	ready      chan struct{}
	readyErr   error
	generation int
	live       map[string]notifyReceiver
}

// FreedesktopNotifier delivers notifications over org.freedesktop.Notifications on a private session bus connection
type FreedesktopNotifier struct {
	mu         sync.Mutex
	connection *notifyConnection
	retryAfter time.Time
	pending    int
}

// Notifier over the session notification daemon
func NewFreedesktopNotifier() *FreedesktopNotifier { return &FreedesktopNotifier{} }

// The D-Bus notifier needs no platform probe
func (n *FreedesktopNotifier) Supported() bool { return true }

// A daemon close ends the notification
func (n *FreedesktopNotifier) ReleaseOnClose() bool { return true }

func sessionBusAddress() string {
	if v := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); v != "" {
		return v
	}
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return "unix:path=" + runtime + "/bus"
}

// Drops a dead connection, failing every live notification on it
func (n *FreedesktopNotifier) disconnectLocked(state *notifyConnection) {
	if n.connection != state {
		return
	}
	n.connection = nil
	state.generation++
	live := state.live
	state.live = map[string]notifyReceiver{}
	for _, item := range live {
		go item.receive("failed", nil)
	}
	state.conn.Close()
}

// Shared connection with the notification signal and owner change matches installed
func (n *FreedesktopNotifier) connectLocked() *notifyConnection {
	if n.connection != nil {
		return n.connection
	}
	state := &notifyConnection{ready: make(chan struct{}), live: map[string]notifyReceiver{}}
	n.connection = state
	go func() {
		defer close(state.ready)
		ctx, cancel := context.WithTimeout(context.Background(), notifyDeliveryTimeout)
		defer cancel()
		conn, err := dbus.Dial(sessionBusAddress(), dbus.WithContext(ctx))
		if err == nil {
			if err = conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err == nil {
				err = conn.Hello()
			}
		}
		if err == nil {
			for _, rule := range []string{
				"type='signal',interface='" + notifyService + "',path='" + string(notifyPath) + "'",
				"type='signal',sender='" + dbusService + "',interface='" + dbusService + "',member='NameOwnerChanged',arg0='" + notifyService + "'",
			} {
				if err = conn.BusObject().CallWithContext(ctx, dbusService+".AddMatch", 0, rule).Err; err != nil {
					break
				}
			}
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		if err != nil {
			state.readyErr = err
			if conn != nil {
				state.conn = conn
				n.disconnectLocked(state)
			} else if n.connection == state {
				n.connection = nil
			}
			return
		}
		state.conn = conn
		signals := make(chan *dbus.Signal, 64)
		conn.Signal(signals)
		go n.watch(state, signals)
	}()
	return state
}

// Routes daemon signals to live notifications and fails them when the daemon changes owner or the bus closes
func (n *FreedesktopNotifier) watch(state *notifyConnection, signals chan *dbus.Signal) {
	for signal := range signals {
		n.mu.Lock()
		if signal.Sender == dbusService && signal.Name == dbusService+".NameOwnerChanged" && len(signal.Body) >= 2 && signal.Body[0] == notifyService {
			state.generation++
			old, _ := signal.Body[1].(string)
			for _, item := range state.live {
				if item.owner == old {
					go item.receive("failed", nil)
				}
			}
			n.mu.Unlock()
			continue
		}
		if signal.Path != notifyPath || len(signal.Name) <= len(notifyService)+1 || signal.Name[:len(notifyService)] != notifyService {
			n.mu.Unlock()
			continue
		}
		member := signal.Name[len(notifyService)+1:]
		var id, value any
		if len(signal.Body) > 0 {
			id = signal.Body[0]
		}
		if len(signal.Body) > 1 {
			value = signal.Body[1]
		}
		item, ok := state.live[fmt.Sprintf("%s:%v", signal.Sender, id)]
		n.mu.Unlock()
		if ok {
			item.receive(member, value)
		}
	}
	n.mu.Lock()
	n.disconnectLocked(state)
	n.mu.Unlock()
}

type freedesktopNotification struct {
	n         *FreedesktopNotifier
	o         NotificationOptions
	ev        NotificationEvents
	mu        sync.Mutex
	delivered *struct {
		state *notifyConnection
		owner string
		id    uint32
	}
	finished bool
	ctx      context.Context
	abort    context.CancelFunc
}

// Notification that delivers on Show
func (n *FreedesktopNotifier) Create(o NotificationOptions, ev NotificationEvents) Notification {
	ctx, abort := context.WithCancel(context.Background())
	return &freedesktopNotification{n: n, o: o, ev: ev, ctx: ctx, abort: abort}
}

func (f *freedesktopNotification) release() {
	f.mu.Lock()
	f.finished = true
	target := f.delivered
	f.delivered = nil
	f.mu.Unlock()
	if target != nil {
		f.n.mu.Lock()
		delete(target.state.live, fmt.Sprintf("%s:%d", target.owner, target.id))
		f.n.mu.Unlock()
	}
}

// Withdraws the notification from the daemon
func (f *freedesktopNotification) Close() {
	f.mu.Lock()
	target := f.delivered
	f.mu.Unlock()
	f.abort()
	f.release()
	if target == nil || target.state.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyDeliveryTimeout)
	defer cancel()
	target.state.conn.Object(target.owner, notifyPath).CallWithContext(ctx, notifyService+".CloseNotification", 0, target.id)
}

func (f *freedesktopNotification) fail() bool {
	f.release()
	f.ev.Failed()
	return false
}

// Resolves the daemon owner, starts the service when it has none, reads capabilities and sends Notify
func (f *freedesktopNotification) Show() bool {
	n := f.n
	f.mu.Lock()
	finished := f.finished
	f.mu.Unlock()
	n.mu.Lock()
	if finished || time.Now().Before(n.retryAfter) || n.pending >= notifyMaxPending {
		n.mu.Unlock()
		return f.fail()
	}
	n.pending++
	state := n.connectLocked()
	n.mu.Unlock()
	ctx, cancel := context.WithTimeout(f.ctx, notifyDeliveryTimeout)
	defer func() {
		cancel()
		n.mu.Lock()
		n.pending--
		n.mu.Unlock()
	}()
	addressed := -1
	err := f.deliver(ctx, state, &addressed)
	if err == nil {
		return true
	}
	n.mu.Lock()
	if addressed < 0 || state.generation == addressed {
		n.retryAfter = time.Now().Add(notifyRetryCooldown)
	}
	n.mu.Unlock()
	return f.fail()
}

func (f *freedesktopNotification) deliver(ctx context.Context, state *notifyConnection, addressed *int) error {
	n := f.n
	select {
	case <-state.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	if state.readyErr != nil {
		return state.readyErr
	}
	bus := state.conn.BusObject()
	getOwner := func() (string, int, error) {
		var owner string
		err := bus.CallWithContext(ctx, dbusService+".GetNameOwner", 0, notifyService).Store(&owner)
		n.mu.Lock()
		generation := state.generation
		n.mu.Unlock()
		return owner, generation, err
	}
	owner, generation, err := getOwner()
	if err != nil {
		var dbusErr dbus.Error
		if !errors.As(err, &dbusErr) || dbusErr.Name != "org.freedesktop.DBus.Error.NameHasNoOwner" {
			return err
		}
		if err := bus.CallWithContext(ctx, dbusService+".StartServiceByName", 0, notifyService, uint32(0)).Err; err != nil {
			return err
		}
		if owner, generation, err = getOwner(); err != nil {
			return err
		}
	}
	*addressed = generation
	current := func() int {
		n.mu.Lock()
		defer n.mu.Unlock()
		return state.generation
	}
	if current() != generation {
		return errors.New("Notification owner changed during lookup")
	}
	daemon := state.conn.Object(owner, notifyPath)
	var caps []string
	if err := daemon.CallWithContext(ctx, notifyService+".GetCapabilities", 0).Store(&caps); err != nil {
		return err
	}
	if current() != generation {
		return errors.New("Notification owner changed before delivery")
	}
	actions := []string{}
	if _, ubuntu := os.LookupEnv("ELECTRON_USE_UBUNTU_NOTIFIER"); slices.Contains(caps, "actions") && !ubuntu {
		actions = append(actions, "default", "View")
		for i, text := range f.o.Actions {
			actions = append(actions, strconv.Itoa(i), text)
		}
	}
	hints := map[string]dbus.Variant{
		"urgency":        dbus.MakeVariant(byte(1)),
		"desktop-entry":  dbus.MakeVariant("icebath"),
		"suppress-sound": dbus.MakeVariant(f.o.Silent),
	}
	var id uint32
	if err := daemon.CallWithContext(ctx, notifyService+".Notify", 0, "Icebath", uint32(0), f.o.Icon, f.o.Title, f.o.Body, actions, hints, int32(-1)).Store(&id); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if state.generation != generation {
		return errors.New("Notification owner changed during delivery")
	}
	if id == 0 {
		return fmt.Errorf("Notify returned an invalid id: %d", id)
	}
	f.mu.Lock()
	f.delivered = &struct {
		state *notifyConnection
		owner string
		id    uint32
	}{state, owner, id}
	f.mu.Unlock()
	state.live[fmt.Sprintf("%s:%d", owner, id)] = notifyReceiver{owner: owner, receive: func(member string, value any) {
		key, isString := value.(string)
		isActionKey := false
		for i := 0; i < len(actions); i += 2 {
			if actions[i] == key {
				isActionKey = true
			}
		}
		switch {
		case member == "ActionInvoked" && isString && isActionKey:
			if key == "default" {
				f.ev.Click()
			} else if index, err := strconv.Atoi(key); err == nil && actionKeyDigits.MatchString(key) && index < len(f.o.Actions) {
				f.ev.Action(index)
			} else {
				return
			}
			f.Close()
		case member == "NotificationClosed" || member == "failed":
			f.release()
			if member == "failed" {
				f.ev.Failed()
			} else {
				f.ev.Close()
			}
		}
	}}
	return nil
}

// Closes the shared connection at quit
func (n *FreedesktopNotifier) Dispose() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.connection != nil && n.connection.conn != nil {
		n.connection.conn.Close()
	}
}
