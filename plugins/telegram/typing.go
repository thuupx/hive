package telegram

import (
	"context"
	"time"
)

// A turn shows that it is working two ways: Telegram's transient chat action,
// and a ✍️ reaction on the user's own message.
//
// The chat action shows in the header and fades on its own, so it is re-sent
// while a turn runs and simply forgotten when the turn ends — there is no
// delete step for it. The reaction shows inside the conversation, which is
// where the header is easy to miss, especially in a forum topic; it does not
// fade, so it is removed when the turn ends. Telegram has no in-chat typing
// bubble like some platforms: a reaction is what the Bot API offers.
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

// typingReaction is the reaction a turn leaves on the message that started
// it: a writing hand, the closest thing Telegram has to "is typing".
const typingReaction = "✍️"

// typing is one conversation showing the working signal.
type typing struct {
	conversation string

	// messageID is the user's message the reaction sits on. It is zero when
	// the turn was not started by a message — a button press, say — and there
	// is nothing to take the reaction off.
	messageID int64

	started time.Time
}

// startTyping shows that a turn is working.
//
// The signal is sent before the delivery is dispatched, because the agent can
// emit its first event before the delivery returns, and the signal has to
// come first in the conversation.
func (p *Plugin) startTyping(ctx context.Context, conversationID string, messageID int64) {
	if !p.opts.TypingIndicator || conversationID == "" {
		return
	}

	chatID, threadID := splitConversation(conversationID)
	if messageID != 0 {
		if err := p.client.SetMessageReaction(ctx, chatID, messageID, typingReaction); err != nil {
			// Reactions can be off in a group; the chat action still shows.
			p.log.Debug("could not react to the message", "error", err)
		}
	}
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
		messageID:    messageID,
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
	var expired []*typing
	for key, entry := range p.typing {
		// A turn that ended without an answer stops refreshing, and its
		// reaction comes off rather than claiming work that is not happening.
		if time.Since(entry.started) > typingTimeout {
			delete(p.typing, key)
			expired = append(expired, entry)
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

	for _, entry := range expired {
		p.clearTypingReaction(ctx, entry)
	}
}

// clearTypingReaction takes the working reaction off the user's message.
func (p *Plugin) clearTypingReaction(ctx context.Context, entry *typing) {
	if entry.messageID == 0 {
		return
	}
	chatID, _ := splitConversation(entry.conversation)
	if err := p.client.SetMessageReaction(ctx, chatID, entry.messageID, ""); err != nil {
		// A reaction left behind is cosmetic. Failing the turn over it is not.
		p.log.Debug("could not remove the typing reaction",
			"conversation", entry.conversation, "error", err)
	}
}

// stopTyping takes the signal down for a conversation.
//
// It is called when the turn the signal stood for has something to say: an
// answer, a failure, or a cancel. The chat action fades on its own; the
// reaction is removed so a stale "typing" cannot sit on the user's message.
func (p *Plugin) stopTyping(ctx context.Context, conversationID string) {
	p.mu.Lock()
	entry, ok := p.typing[conversationID]
	if ok {
		delete(p.typing, conversationID)
	}
	p.mu.Unlock()

	if ok {
		p.clearTypingReaction(ctx, entry)
	}
}
