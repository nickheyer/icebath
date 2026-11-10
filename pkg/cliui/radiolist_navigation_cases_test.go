package cliui

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRadiolistDispatchesContextualBackNavigation(t *testing.T) {
	wentBack := errors.New("went back")
	var events []MenuNavigationEvent
	h := func(ev MenuNavigationEvent, _ ...any) (*MenuNavigationStart, error) {
		events = append(events, ev)
		if ev == NavEventBack {
			return nil, wentBack
		}
		if ev == NavEventBegin {
			return &MenuNavigationStart{AllowBack: true}, nil
		}
		return nil, nil
	}
	c, _ := testConsole("", &fakeScreen{keys: []int{KeyLeft}})
	_, err := c.CursesRadiolist(WithMenuNavigationHandler(context.Background(), h), "Pick", items("a", "b"), 0, RadiolistOptions{})
	if !errors.Is(err, wentBack) || !reflect.DeepEqual(events, []MenuNavigationEvent{NavEventBegin, NavEventBack}) {
		t.Fatalf("err %v events %v", err, events)
	}
}

func TestRadiolistDispatchesContextualEscapeCancellation(t *testing.T) {
	cancelled := errors.New("cancelled")
	h := func(ev MenuNavigationEvent, _ ...any) (*MenuNavigationStart, error) {
		if ev == NavEventCancel {
			return nil, cancelled
		}
		if ev == NavEventBegin {
			return &MenuNavigationStart{}, nil
		}
		return nil, nil
	}
	c, _ := testConsole("", &fakeScreen{keys: keys("\x1b")})
	if _, err := c.CursesRadiolist(WithMenuNavigationHandler(context.Background(), h), "Pick", items("a", "b"), 0, RadiolistOptions{}); !errors.Is(err, cancelled) {
		t.Fatalf("err %v", err)
	}
}
