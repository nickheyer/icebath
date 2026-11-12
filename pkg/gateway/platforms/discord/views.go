package discord

import (
	"context"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// Component callbacks of live views keyed by message id and custom id, the empty message id matching any
type viewStore struct {
	mu    sync.Mutex
	items map[[2]string]func(ctx context.Context, i *discordgo.Interaction)
}

// Registers a component callback for a custom id, bound to one message when the id is set
func (v *viewStore) add(messageID, customID string, fn func(ctx context.Context, i *discordgo.Interaction)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.items == nil {
		v.items = map[[2]string]func(context.Context, *discordgo.Interaction){}
	}
	v.items[[2]string{messageID, customID}] = fn
}

// Drops the component callbacks of a custom id set
func (v *viewStore) remove(messageID string, customIDs ...string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, id := range customIDs {
		delete(v.items, [2]string{messageID, id})
	}
}

// Callback for a component on a message, the message bound entry before the unbound one
func (v *viewStore) lookup(messageID, customID string) func(ctx context.Context, i *discordgo.Interaction) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if fn, ok := v.items[[2]string{messageID, customID}]; ok {
		return fn
	}
	return v.items[[2]string{"", customID}]
}

// Routes a component or modal interaction to the view item that owns its custom id
func (a *DiscordAdapter) dispatchComponent(ctx context.Context, i *discordgo.Interaction) {
	customID, messageID := "", ""
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		customID = i.MessageComponentData().CustomID
	case discordgo.InteractionModalSubmit:
		customID = i.ModalSubmitData().CustomID
	}
	if i.Message != nil {
		messageID = i.Message.ID
	}
	if fn := a.views.lookup(messageID, customID); fn != nil {
		fn(ctx, i)
	}
}
