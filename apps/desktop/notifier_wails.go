//go:build darwin || windows

package main

import (
	"runtime"
	"strconv"
	"sync"

	"github.com/nickheyer/icebath/pkg/desktop/system"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

// wailsNotifier shows notifications through the Wails service, UNUserNotificationCenter on macOS and toasts on Windows
type wailsNotifier struct {
	svc      *notifications.NotificationService
	mu       sync.Mutex
	seq      int
	handlers map[string]system.NotificationEvents
}

var sharedNotifier = sync.OnceValue(func() *wailsNotifier {
	n := &wailsNotifier{svc: notifications.New(), handlers: map[string]system.NotificationEvents{}}
	n.svc.OnNotificationResponse(n.respond)
	return n
})

func nativeNotifier() system.Notifier { return sharedNotifier() }

// Registers the Wails notification service so it starts with the app
func (s *Shell) registerSystemNatives() {
	s.app.RegisterService(application.NewService(sharedNotifier().svc))
}

// Electron shows native notifications on macOS and Windows 10 and later
func (n *wailsNotifier) Supported() bool { return true }

// A dismissed toast or banner does not end the notification
func (n *wailsNotifier) ReleaseOnClose() bool { return false }

// Routes a click on the body to Click and a button to Action by its index id
func (n *wailsNotifier) respond(result notifications.NotificationResult) {
	if result.Error != nil {
		return
	}
	n.mu.Lock()
	ev, ok := n.handlers[result.Response.ID]
	n.mu.Unlock()
	if !ok {
		return
	}
	if result.Response.ActionIdentifier == notifications.DefaultActionIdentifier {
		ev.Click()
		return
	}
	if index, err := strconv.Atoi(result.Response.ActionIdentifier); err == nil {
		ev.Action(index)
	}
}

type wailsNotification struct {
	n  *wailsNotifier
	id string
	o  system.NotificationOptions
	ev system.NotificationEvents
}

func (n *wailsNotifier) Create(o system.NotificationOptions, ev system.NotificationEvents) system.Notification {
	n.mu.Lock()
	n.seq++
	id := "icebath-" + strconv.Itoa(n.seq)
	n.mu.Unlock()
	return &wailsNotification{n: n, id: id, o: o, ev: ev}
}

// Delivers the notification, reporting a failure through Failed as Electron does after show returns
func (w *wailsNotification) Show() bool {
	svc := w.n.svc
	if authorized, err := svc.CheckNotificationAuthorization(); err != nil || !authorized {
		if granted, err := svc.RequestNotificationAuthorization(); err != nil || !granted {
			w.ev.Failed()
			return true
		}
	}
	w.n.mu.Lock()
	w.n.handlers[w.id] = w.ev
	w.n.mu.Unlock()
	options := notifications.NotificationOptions{ID: w.id, Title: w.o.Title, Body: w.o.Body, Sound: &notifications.NotificationSound{Silent: w.o.Silent}}
	if w.o.Icon != "" && runtime.GOOS == "windows" {
		options.Attachments = []notifications.NotificationAttachment{{Path: w.o.Icon, Type: "appLogoOverride"}}
	}
	var err error
	if len(w.o.Actions) > 0 {
		category := notifications.NotificationCategory{ID: w.id}
		for i, text := range w.o.Actions {
			category.Actions = append(category.Actions, notifications.NotificationAction{ID: strconv.Itoa(i), Title: text})
		}
		if err = svc.RegisterNotificationCategory(category); err == nil {
			options.CategoryID = w.id
			err = svc.SendNotificationWithActions(options)
		}
	} else {
		err = svc.SendNotification(options)
	}
	if err != nil {
		w.forget()
		w.ev.Failed()
	}
	return true
}

func (w *wailsNotification) forget() {
	w.n.mu.Lock()
	delete(w.n.handlers, w.id)
	w.n.mu.Unlock()
	if len(w.o.Actions) > 0 {
		w.n.svc.RemoveNotificationCategory(w.id)
	}
}

// Withdraws the notification from the notification center
func (w *wailsNotification) Close() {
	w.n.svc.RemoveNotification(w.id)
	w.forget()
}
