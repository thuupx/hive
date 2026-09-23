package slack

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// A turn shows that it is working by animating one message, and the answer
// replaces that message when it arrives.
//
// Slack has no typing indicator a bot can send, so the indicator is a message
// that says nothing and means "working". Making it the message the answer replaces
// is what keeps a turn to one message per thing it has to say, instead of an
// acknowledgement followed by an answer that repeats it.
const (
	// typingInterval is how often the indicator advances. Slack rate-limits a
	// channel to about one update a second, so this is comfortably inside it.
	typingInterval = 3 * time.Second

	// typingTimeout is how long a turn may show the indicator before it is assumed
	// to have ended without an answer. A clock that ticks forever is a lie.
	typingTimeout = 30 * time.Minute

	// maxTyping bounds how many turns may be animating at once.
	maxTyping = 32
)

// typingFrames are the animation. A clock ticks, which is legible at one
// character and needs no words.
var typingFrames = []string{
	":clock1:", ":clock2:", ":clock3:", ":clock4:", ":clock5:", ":clock6:",
	":clock7:", ":clock8:", ":clock9:", ":clock10:", ":clock11:", ":clock12:",
}

// typing is one message that is showing a turn is working.
type typing struct {
	conversation string
	thread       string
	timestamp    string
	frame        int
	started      time.Time
}

// startTyping shows that a turn is working, and returns nothing if it cannot.
//
// The message is posted before the delivery is dispatched, because the agent can
// emit its first tool call before the delivery returns, and the indicator has to
// come first in the conversation.
func (p *Plugin) startTyping(ctx context.Context, conversationID, thread string) {
	if !p.opts.TypingIndicator {
		return
	}

	timestamp, err := p.client.PostMessage(ctx, PostMessageRequest{
		Channel:  channelOf(conversationID),
		ThreadTS: thread,
		Message:  textMessage(typingFrames[0]),
	})
	if err != nil {
		// An indicator that cannot be shown is not a reason to fail the turn.
		p.log.Debug("could not show the typing indicator", "error", err)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// A long-lived transport must not accumulate indicators for turns that ended
	// without an answer.
	for key, entry := range p.typing {
		if len(p.typing) < maxTyping {
			break
		}
		if time.Since(entry.started) > typingTimeout || len(p.typing) >= maxTyping {
			delete(p.typing, key)
		}
	}

	p.typing[p.typingKey(conversationID)] = &typing{
		conversation: conversationID,
		thread:       thread,
		timestamp:    timestamp,
		started:      time.Now(),
	}
}

// typingKey is what a typing message is tracked by.
//
// The conversation is the key because the run is not known until the delivery
// returns, and the indicator has to be posted before that.
func (p *Plugin) typingKey(conversationID string) string {
	return conversationID
}

// tickTyping animates every working turn until the context ends.
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

// advanceTyping moves each indicator on by one frame.
func (p *Plugin) advanceTyping(ctx context.Context) {
	p.mu.Lock()
	active := make([]*typing, 0, len(p.typing))
	for key, entry := range p.typing {
		// A turn that ended without an answer stops ticking, and the message is
		// left as it was rather than pretending it is still working.
		if time.Since(entry.started) > typingTimeout {
			delete(p.typing, key)
			continue
		}
		entry.frame++
		active = append(active, entry)
	}
	p.mu.Unlock()

	for _, entry := range active {
		frame := typingFrames[entry.frame%len(typingFrames)]
		if err := p.client.UpdateMessage(ctx, UpdateMessageRequest{
			Channel:   channelOf(entry.conversation),
			Timestamp: entry.timestamp,
			Message:   textMessage(frame),
		}); err != nil {
			p.log.Debug("could not advance the typing indicator", "error", err)
		}
	}
}

// takeTyping stops the indicator for a conversation, returning the message that
// was showing it.
//
// The caller replaces that message, so the turn ends with one message where the
// indicator was, rather than two.
func (p *Plugin) takeTyping(conversationID string) *typing {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := p.typingKey(conversationID)
	entry, ok := p.typing[key]
	if !ok {
		return nil
	}
	delete(p.typing, key)
	return entry
}

// replaceTyping edits the indicator into what the turn produced.
func (p *Plugin) replaceTyping(ctx context.Context, conversationID string, message Message) bool {
	entry := p.takeTyping(conversationID)
	if entry == nil {
		return false
	}

	if err := p.client.UpdateMessage(ctx, UpdateMessageRequest{
		Channel:   channelOf(entry.conversation),
		Timestamp: entry.timestamp,
		Message:   message,
	}); err != nil {
		p.log.Warn("could not replace the typing indicator",
			"conversation", conversationID, "error", err)
		return false
	}
	return true
}

// isAnswer is whether an event is the answer to a turn.
//
// Only an answer replaces the indicator. A tool card is a step, and it belongs
// after the indicator, not in place of it.
func isAnswer(ev v1.Event) bool {
	if ev.Type != v1.EventMessage {
		return false
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false
	}
	return strings.TrimSpace(payload.Text) != ""
}
