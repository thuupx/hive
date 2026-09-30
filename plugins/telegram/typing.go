package telegram

import (
	"context"
	"time"
)

// A turn shows that it is working with Telegram's transient chat action.
//
// Unlike a message, the action is not something the bot can take down: it
// fades on its own. So the transport re-sends it while a turn runs and simply
// forgets it when the turn ends. That is why there is no delete step here,
// and why a signal left behind is impossible.
const (
	// typingInterval is how often the action is refreshed. Telegram shows it
	// for about five seconds, so it is re-sent well inside that window.
	typingInterval = 4 * time.Second

	// typingTimeout is how long a turn may show the signal before it is
	// assumed to have ended without an answer. A signal that refreshes
	// forever is a lie.
	typingTimeout = 30 * time.Minute

	// maxTyping bounds how many turns may be animating at once.
	maxTyping = 32
)

// typing is one conversation showing the working signal.
type typing struct {
	conversation string
	started      time.Time
}

// startTyping shows that a turn is working.
//
// The signal is sent before the delivery is dispatched, because the agent can
// emit its first event before the delivery returns, and the signal has to
// come first in the conversation.
func (p *Plugin) startTyping(ctx context.Context, conversationID string) {
	if !p.opts.TypingIndicator || conversationID == "" {
		return
	}

	chatID, threadID := splitConversation(conversationID)
	if err := p.client.SendChatAction(ctx, chatID, threadID, ActionTyping); err != nil {
		// A signal that cannot be shown is not a reason to fail the turn.
		p.log.Debug("could not show the working signal", "error", err)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// A long-lived transport must not accumulate signals for turns that ended
	// without an answer.
	for key, entry := range p.typing {
		if len(p.typing) < maxTyping {
			break
		}
		if time.Since(entry.started) > typingTimeout || len(p.typing) >= maxTyping {
			delete(p.typing, key)
		}
	}

	p.typing[conversationID] = &typing{
		conversation: conversationID,
		started:      time.Now(),
	}
}

// tickTyping refreshes every working signal until the context ends.
func (p *Plugin) tickTyping(ctx context.Context) {
	ticker := time.NewTicker(typingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.advanceTyping(ctx)
		}
	}
}

// advanceTyping refreshes each active signal.
func (p *Plugin) advanceTyping(ctx context.Context) {
	p.mu.Lock()
	active := make([]*typing, 0, len(p.typing))
	for key, entry := range p.typing {
		// A turn that ended without an answer stops refreshing, and the
		// action fades on its own.
		if time.Since(entry.started) > typingTimeout {
			delete(p.typing, key)
			continue
		}
		active = append(active, entry)
	}
	p.mu.Unlock()

	for _, entry := range active {
		chatID, threadID := splitConversation(entry.conversation)
		if err := p.client.SendChatAction(ctx, chatID, threadID, ActionTyping); err != nil {
			p.log.Debug("could not refresh the working signal", "error", err)
		}
	}
}

// stopTyping forgets the signal for a conversation.
//
// There is nothing to take down: Telegram's action fades by itself.
// Forgetting it stops the refresh, which is what ends it.
func (p *Plugin) stopTyping(conversationID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.typing, conversationID)
}
