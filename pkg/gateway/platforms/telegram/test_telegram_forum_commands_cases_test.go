package telegram

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

// Installs a menu reader double for the test and restores the wiring after
func useMenu(t *testing.T, src InlineCommandSource) {
	prior := InlineCommands
	InlineCommands = src
	t.Cleanup(func() { InlineCommands = prior })
}

// Forum supergroup message of a chat
func forumMessage(chatID int64) *models.Message {
	return &models.Message{Chat: models.Chat{ID: chatID, Type: models.ChatTypeSupergroup, IsForum: true}}
}

func TestEnsureForumCommandsRegistersOnce(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	useMenu(t, &fakeInlineCommands{menu: []CatalogItem{{"new", "Start new session"}, {"help", "Show help"}}, menuMax: 60})
	a.ensureForumCommands(context.Background(), forumMessage(-123))
	if !a.forumCommandRegistered[-123] {
		t.Fatal("forum chat not marked registered")
	}
	calls := fake.callsOf("setMyCommands")
	if len(calls) != 1 || calls[0].params["scope"] != `{"type":"chat","chat_id":-123}` {
		t.Fatalf("calls %+v", calls)
	}
}

func TestEnsureForumCommandsRaceSafety(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	useMenu(t, &fakeInlineCommands{menu: []CatalogItem{{"new", "Start new session"}}, menuMax: 60})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.ensureForumCommands(context.Background(), forumMessage(-789))
		}()
	}
	wg.Wait()
	if n := len(fake.callsOf("setMyCommands")); n != 1 {
		t.Fatalf("setMyCommands calls %d", n)
	}
}

// Menu reader that blocks until the dispatcher ticked, the slow skill scan of hermes
type blockingMenu struct {
	fakeInlineCommands
	scanStarted, ticked chan struct{}
	once                sync.Once
}

func (b *blockingMenu) TelegramMenuCommands(context.Context, int) ([]CatalogItem, int, error) {
	b.once.Do(func() { close(b.scanStarted) })
	select {
	case <-b.ticked:
		return []CatalogItem{{"help", "Show help"}}, 0, nil
	case <-time.After(time.Second):
		return nil, 0, nil
	}
}

// Whether the menu build left the dispatcher free and every registration carried the help command
func menuBuildLeavesLoopFree(t *testing.T, fake *scriptedBot, run func()) bool {
	menu := &blockingMenu{fakeInlineCommands: fakeInlineCommands{menuMax: 60}, scanStarted: make(chan struct{}), ticked: make(chan struct{})}
	useMenu(t, menu)
	done := make(chan struct{})
	go func() {
		run()
		close(done)
	}()
	select {
	case <-menu.scanStarted:
	case <-time.After(time.Second):
	}
	close(menu.ticked)
	<-done
	calls := fake.callsOf("setMyCommands")
	for _, c := range calls {
		if c.params["commands"] != `[{"command":"help","description":"Show help"}]` {
			return false
		}
	}
	return len(calls) > 0
}

func TestCommandMenuSkillScanRunsOffTheEventLoop(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	if !menuBuildLeavesLoopFree(t, fake, func() { _ = a.registerCommandMenu(context.Background()) }) {
		t.Fatal("post-connect registration blocked the dispatcher")
	}
	forum, forumFake := newScriptedAdapter(t, nil)
	if !menuBuildLeavesLoopFree(t, forumFake, func() { forum.ensureForumCommands(context.Background(), forumMessage(-123)) }) {
		t.Fatal("lazy forum registration blocked the dispatcher")
	}
}
