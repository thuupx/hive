package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/thupham/hive/plugins/sdk"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// Options configures the Slack plugin.
type Options struct {
	// BotUserID is the bot's own Slack user id, used to resolve mentions.
	BotUserID string

	// RequireMention ignores messages that do not address the bot. A channel
	// where Hive is the only participant can turn it off.
	RequireMention bool

	// Acknowledgement is the optional immediate visual feedback.
	Acknowledgement Acknowledgement

	// ChannelContext is how many recent messages of a conversation to hand to the
	// agent, so a prompt means what it means in the room. Zero disables it.
	ChannelContext int

	// MaxAttachmentBytes bounds how large a file the transport will read. Zero
	// uses the default.
	MaxAttachmentBytes int64

	Log *slog.Logger
}

// Acknowledgement is the optional "Hive received this" signal.
//
// It means the message was received and recognized. It must never be read as
// agent started, working, or finished. Its failure cannot fail the operation.
type Acknowledgement struct {
	Enabled bool

	// Mode is how the acknowledgement is shown. Reaction is the default, and none
	// means the signal is off even when it is configured on.
	Mode string

	Reaction string
}

// delivery is one inbound platform delivery plus the coordinates needed to reply.
type delivery struct {
	envelope       v1.Envelope
	conversationID string
	timestamp      string
}

// Plugin wires Slack into a Hive transport plugin.
type Plugin struct {
	host   *sdk.Host
	client Client
	parser Parser
	opts   Options
	log    *slog.Logger

	mu    sync.Mutex
	bound map[string]string // conversation id to session id

	// toolMessages maps a tool call to the message that represents it, so the
	// call is one message instead of one per update.
	//
	// It is bounded: a long-lived transport must not remember every tool call it
	// has ever shown.
	toolMessages map[string]string

	// toolOrder is the order tool messages were created in, so the oldest can be
	// forgotten.
	toolOrder []string

	// handled remembers recent deliveries, because one platform message can
	// arrive as more than one event.
	handled map[string]time.Time

	// cursors is how far each session has been accepted, read at startup so a
	// restart can replay what was missed.
	cursors map[string]int64
}

// New returns a Slack plugin over a connected host.
func New(host *sdk.Host, client Client, opts Options) *Plugin {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Plugin{
		host:         host,
		client:       client,
		parser:       Parser{BotUserID: opts.BotUserID, RequireMention: opts.RequireMention},
		opts:         opts,
		log:          log,
		bound:        make(map[string]string),
		toolMessages: make(map[string]string),
		handled:      make(map[string]time.Time),
		cursors:      make(map[string]int64),
	}
}

// Run serves Slack until ctx is cancelled or the connection ends.
func (p *Plugin) Run(ctx context.Context) error {
	if err := p.reloadBindings(ctx); err != nil {
		p.log.Warn("could not load bound sessions", "error", err)
	}

	// A transport that was down while the agent worked would otherwise lose what
	// it missed. The cursor is what it kept for exactly this.
	p.catchUp(ctx)

	// Subscribe to every session this transport is authorized for, and filter
	// while rendering.
	subscription, err := p.host.Subscribe(ctx, v1.SubscribeRequest{})
	if err != nil {
		return fmt.Errorf("slack: subscribe to events: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Rendering talks to the platform, which is slow, and the delivery channel is
	// bounded. Draining it must never wait for a post, or a burst of agent events
	// overflows the buffer and the events are lost.
	render := make(chan v1.DeliveredEvent, renderQueue)
	go p.renderWorker(ctx, subscription.SubscriptionID, render)
	go p.drain(ctx, render)

	// The SDK loop is what dispatches core-invoked methods and delivers subscribed
	// events. A transport that subscribes without running it receives nothing:
	// the subscription exists in the core and the delivered events are never read.
	//
	// Either loop ending ends the plugin, so a dead connection is not a silent
	// half-running transport.
	ended := make(chan error, 2)
	go func() { ended <- p.host.Run(ctx) }()
	go func() { ended <- p.serveInbound(ctx) }()

	err = <-ended
	cancel()
	return err
}

// dispatch hands a delivered event to the render worker.
//
// The drain loop is what keeps delivery flowing, so it must not block on a
// network call. A full queue is reported rather than silently waited on.
func (p *Plugin) dispatch(ctx context.Context, render chan<- v1.DeliveredEvent, delivered v1.DeliveredEvent) {
	select {
	case render <- delivered:
	case <-ctx.Done():
	default:
		p.log.Warn("rendering is behind; an event was not shown",
			"session", delivered.Event.SessionID, "sequence", delivered.Event.Sequence)
	}
}

// drain delivers subscribed events to the render worker.
func (p *Plugin) drain(ctx context.Context, render chan<- v1.DeliveredEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.host.Done():
			return
		case delivered, ok := <-p.host.Events():
			if !ok {
				return
			}
			p.dispatch(ctx, render, delivered)
		}
	}
}

// serveInbound reads platform deliveries, normalizes them, and renders the
// outcome.
func (p *Plugin) serveInbound(ctx context.Context) error {
	events, err := p.client.Events(ctx)
	if err != nil {
		return err
	}
	p.log.Info("slack socket mode connected")

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.host.Done():
			return nil
		case inbound, ok := <-events:
			if !ok {
				return errors.New("slack: the socket mode connection ended")
			}
			p.handleInbound(ctx, inbound)
		}
	}
}

func (p *Plugin) handleInbound(ctx context.Context, inbound Inbound) {
	d, ok := p.parse(inbound)
	if !ok {
		return
	}

	// Slack describes one message with both an app_mention and a message event
	// when it mentions the bot, so the same delivery arrives twice. Hive would
	// deduplicate the command, but the user would see two acknowledgements.
	if p.alreadyHandled(d.envelope.SourceID) {
		return
	}

	// The catalog is the transport's own, so help is answered without asking the
	// core. Sending it through Hive would mean the transport does not know what it
	// exposes.
	if d.envelope.Command != nil && d.envelope.Command.Name == HelpCommand {
		p.post(ctx, d.conversationID, "", HelpMessage())
		return
	}

	p.fetchAttachments(ctx, &d.envelope)

	// Acknowledge as early as the transport can safely confirm acceptance, and
	// before waiting for the agent. The reaction is best-effort: failing to add
	// it must not fail the operation.
	p.acknowledge(ctx, d)

	var outcome v1.TransportOutcome
	err := p.host.Call(ctx, v1.MethodTransportInbound, wireParams(d.envelope), &outcome)
	if err != nil {
		p.log.Warn("inbound delivery failed", "error", err)
		p.post(ctx, d.conversationID, "", textMessage(fmt.Sprintf(":warning: %s", err.Error())))
		return
	}

	if outcome.SessionID != "" {
		p.rememberBinding(d.conversationID, outcome.SessionID)
	}
	p.post(ctx, d.conversationID, "", RenderOutcome(outcome))
}

// fetchAttachments reads the files a user sent.
//
// Reading the platform is the transport's job, so the bytes are fetched here and
// only the bytes travel onward. A file that cannot be read is dropped with a note
// rather than failing the message: the text is still worth handling.
func (p *Plugin) fetchAttachments(ctx context.Context, env *v1.Envelope) {
	if env.Message == nil || len(env.Message.Attachments) == 0 {
		return
	}

	kept := make([]v1.Attachment, 0, len(env.Message.Attachments))
	for _, attachment := range env.Message.Attachments {
		data, err := p.client.DownloadFile(ctx, attachment.URL, p.opts.MaxAttachmentBytes)
		if err != nil {
			p.log.Warn("could not read an attachment",
				"name", attachment.Name, "error", err)
			continue
		}

		attachment.Data = data
		attachment.URL = ""
		kept = append(kept, attachment)
	}
	env.Message.Attachments = kept
}

// readRoom reads what else is being said in the conversation.
//
// A channel is not a private pipe: "do it" means whatever it means in the room.
// Reading the platform is the transport's job, and it is bounded because the
// context costs tokens on every turn.
func (p *Plugin) readRoom(ctx context.Context, d delivery) []v1.ChannelMessage {
	if p.opts.ChannelContext <= 0 || d.conversationID == "" {
		return nil
	}

	history, err := p.client.ChannelHistory(ctx, d.conversationID, p.opts.ChannelContext)
	if err != nil {
		// A missing history is not a reason to refuse the message.
		p.log.Debug("could not read the conversation", "conversation", d.conversationID, "error", err)
		return nil
	}

	// The platform returns newest first and includes the message being handled.
	context := make([]v1.ChannelMessage, 0, len(history))
	for _, message := range history {
		if message.Timestamp == d.timestamp {
			continue
		}
		context = append(context, v1.ChannelMessage{
			Author: message.Author,
			Text:   message.Text,
			Self:   message.Self,
		})
	}

	// Oldest first reads as a conversation.
	for i, j := 0, len(context)-1; i < j; i, j = i+1, j-1 {
		context[i], context[j] = context[j], context[i]
	}
	return context
}

// parse turns a platform delivery into an envelope, reporting false when the
// delivery is not for Hive.
func (p *Plugin) parse(inbound Inbound) (delivery, bool) {
	switch inbound.Type {
	case "block_actions":
		var payload ActionPayload
		if err := json.Unmarshal(inbound.Payload, &payload); err != nil {
			return delivery{}, false
		}
		env := p.parser.ParseInteraction(payload)
		return delivery{envelope: env, conversationID: env.ConversationID, timestamp: payload.ActionTS}, true

	default:
		var event MessageEvent
		if err := json.Unmarshal(inbound.Payload, &event); err != nil {
			return delivery{}, false
		}

		// A bot's own message, an edit, or a join is not user intent. A file share
		// is: it is a message the user typed, with something attached.
		if event.BotID != "" || event.User == "" {
			return delivery{}, false
		}
		if event.SubType != "" && event.SubType != "file_share" {
			return delivery{}, false
		}

		env := p.parser.ParseMessage(event)
		if env.Kind == "" {
			// Not addressed to Hive.
			return delivery{}, false
		}
		return delivery{envelope: env, conversationID: event.Channel, timestamp: event.Timestamp}, true
	}
}

// renderQueue bounds how much rendering may be outstanding.
//
// It is generous because the work is a network call and the input is a burst of
// agent events, and it is bounded so a wedged platform cannot grow memory without
// limit.
const renderQueue = 2048

// renderWorker posts Hive events into the conversations that belong to them.
//
// It runs in its own goroutine and in order, so a tool card is created before its
// updates and the drain loop is never blocked by a network call.
func (p *Plugin) renderWorker(ctx context.Context, subscriptionID string, render <-chan v1.DeliveredEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.host.Done():
			return
		case delivered, ok := <-render:
			if !ok {
				return
			}

			// Acknowledge delivery so the core can advance the cursor. Delivery
			// is at-least-once, so an unacknowledged event arrives again.
			conversations := p.conversationsFor(delivered.Event.SessionID)

			// A conversation that shows nothing has to be traceable: this is the
			// line that says whether an event arrived, whether it rendered, and
			// where it went.
			p.log.Debug("event delivered",
				"session", delivered.Event.SessionID,
				"sequence", delivered.Event.Sequence,
				"type", delivered.Event.Type,
				"conversations", len(conversations),
			)
			if len(conversations) == 0 {
				p.log.Warn("an event has no conversation to render into",
					"session", delivered.Event.SessionID,
					"sequence", delivered.Event.Sequence,
					"type", delivered.Event.Type,
				)
			}

			renderer := Renderer{SessionID: delivered.Event.SessionID}
			rendered, ok := renderer.RenderEvent(delivered.Event)
			if ok {
				p.log.Info("rendering an event",
					"session", delivered.Event.SessionID,
					"sequence", delivered.Event.Sequence,
					"type", delivered.Event.Type,
					"conversations", len(conversations),
				)
				for _, conversation := range conversations {
					if rendered.ToolCallID != "" {
						p.renderTool(ctx, conversation, rendered)
						continue
					}
					p.post(ctx, conversation, "", rendered.Message)
				}
			}

			// Acknowledge once per conversation so each durable binding cursor
			// advances. An event with no bound conversation still advances the
			// subscription cursor.
			if len(conversations) == 0 {
				if err := p.host.Ack(ctx, subscriptionID, delivered.Event.Sequence); err != nil {
					p.log.Debug("could not acknowledge an event", "error", err)
				}
				continue
			}
			for _, conversation := range conversations {
				if err := p.host.AckFor(ctx, subscriptionID, delivered.Event.Sequence, conversation); err != nil {
					p.log.Debug("could not acknowledge an event", "conversation", conversation, "error", err)
				}
			}
		}
	}
}

// renderTool keeps one message per tool call.
//
// A tool call produces many updates. Posting each one would bury the conversation,
// so the message is created once and then replaced, and any detail goes in its
// thread.
func (p *Plugin) renderTool(ctx context.Context, conversation string, rendered Rendered) {
	p.mu.Lock()
	timestamp := p.toolMessages[rendered.ToolCallID]
	p.mu.Unlock()

	if timestamp == "" {
		ts, err := p.client.PostMessage(ctx, PostMessageRequest{
			Channel: conversation,
			Message: rendered.Message,
		})
		if err != nil {
			p.log.Warn("could not post a tool call", "conversation", conversation, "error", err)
			return
		}

		p.mu.Lock()
		p.toolMessages[rendered.ToolCallID] = ts
		p.toolOrder = append(p.toolOrder, rendered.ToolCallID)
		for len(p.toolOrder) > maxToolMessages {
			oldest := p.toolOrder[0]
			p.toolOrder = p.toolOrder[1:]
			delete(p.toolMessages, oldest)
		}
		p.mu.Unlock()
		return
	}

	if err := p.client.UpdateMessage(ctx, UpdateMessageRequest{
		Channel:   conversation,
		Timestamp: timestamp,
		Message:   rendered.Message,
	}); err != nil {
		p.log.Warn("could not update a tool call", "conversation", conversation, "error", err)
	}

	if rendered.Detail != "" {
		p.post(ctx, conversation, timestamp, textMessage(rendered.Detail))
	}
}

// alreadyHandled reports whether a delivery was seen recently.
//
// A platform redelivery and a second event describing the same message are the
// same delivery from the conversation's point of view.
func (p *Plugin) alreadyHandled(sourceID string) bool {
	if sourceID == "" {
		return false
	}

	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Bound the memory: a long-lived transport must not grow forever.
	for id, seen := range p.handled {
		if now.Sub(seen) > handledWindow {
			delete(p.handled, id)
		}
	}

	if _, seen := p.handled[sourceID]; seen {
		return true
	}
	p.handled[sourceID] = now
	return false
}

// handledWindow is how long a delivery is remembered.
const handledWindow = 10 * time.Minute

// acknowledge adds the optional "received" reaction.
//
// It is presentation feedback only, and its failure never fails the operation.
func (p *Plugin) acknowledge(ctx context.Context, d delivery) {
	if !p.opts.Acknowledgement.Enabled || p.opts.Acknowledgement.Mode == "none" {
		return
	}

	reaction := p.opts.Acknowledgement.Reaction
	if reaction == "" {
		reaction = "eyes"
	}

	req := ReactionRequest{Channel: d.conversationID, Timestamp: d.timestamp, Name: reaction}
	if err := p.client.AddReaction(ctx, req); err != nil {
		p.log.Debug("acknowledgement failed", "error", err)
	}
}

func (p *Plugin) post(ctx context.Context, conversationID, threadTS string, message Message) {
	if conversationID == "" {
		return
	}
	if _, err := p.client.PostMessage(ctx, PostMessageRequest{
		Channel:  conversationID,
		ThreadTS: threadTS,
		Message:  message,
	}); err != nil {
		p.log.Warn("could not post to Slack", "conversation", conversationID, "error", err)
	}
}

// catchUp renders what was published while this transport was not running.
//
// It is best-effort: a conversation that cannot be caught up is one the user will
// notice, not one that breaks the transport.
func (p *Plugin) catchUp(ctx context.Context) {
	p.mu.Lock()
	cursors := make(map[string]int64, len(p.bound))
	for conversation, sessionID := range p.bound {
		if conversation == sessionID {
			continue
		}
		cursors[sessionID] = p.cursors[sessionID]
	}
	p.mu.Unlock()

	for sessionID, cursor := range cursors {
		conversations := p.conversationsFor(sessionID)
		if len(conversations) == 0 {
			continue
		}

		missed, err := p.replay(ctx, sessionID, cursor)
		if err != nil {
			p.log.Warn("could not catch up a conversation",
				"session", sessionID, "error", err)
			continue
		}
		if missed == 0 {
			continue
		}
		p.log.Info("caught up a conversation", "session", sessionID, "events", missed)
	}
}

// replay renders the events after a cursor.
func (p *Plugin) replay(ctx context.Context, sessionID string, after int64) (int, error) {
	var result v1.EventReplayResult
	if err := p.host.Call(ctx, v1.MethodSessionEvents, v1.EventReplayParams{
		SessionID:    sessionID,
		FromSequence: after,
		Limit:        catchUpLimit,
	}, &result); err != nil {
		return 0, err
	}
	if result.Gap != nil {
		// The cursor was pruned, so the history is gone. Say so rather than
		// pretending the conversation is complete.
		p.log.Warn("the conversation cursor was pruned",
			"session", sessionID, "next", result.Gap.NextSequence)
		return 0, nil
	}

	renderer := Renderer{SessionID: sessionID}
	rendered := 0
	for _, ev := range result.Events {
		message, ok := renderer.RenderEvent(ev)
		if !ok {
			continue
		}
		for _, conversation := range p.conversationsFor(sessionID) {
			if message.ToolCallID != "" {
				p.renderTool(ctx, conversation, message)
				continue
			}
			p.post(ctx, conversation, "", message.Message)
		}
		rendered++
	}
	return rendered, nil
}

// catchUpLimit bounds how much history a restart replays.
const catchUpLimit = 200

// maxToolMessages bounds how many tool cards a transport remembers.
//
// A tool call keeps one message, so this is one entry per tool call the transport
// has shown. Forgetting the oldest means a very old tool card is reposted rather
// than updated, which is a smaller problem than growing forever.
const maxToolMessages = 1024

// reloadBindings rebuilds the conversation-to-session map.
//
// The coordinator owns the bindings, so the transport asks which sessions it can
// reach rather than remembering them across a restart. The conversation side is
// learned from inbound deliveries, which is enough to render.
func (p *Plugin) reloadBindings(ctx context.Context) error {
	var list v1.SessionListResult
	if err := p.host.Call(ctx, v1.MethodSessionList, v1.SessionListParams{}, &list); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, session := range list.Sessions {
		if _, known := p.bound[session.SessionID]; !known {
			// The session is reachable; the conversation is filled in on the
			// first delivery for it.
			p.bound[session.SessionID] = session.SessionID
		}
		p.cursors[session.SessionID] = session.EventCursor
	}
	return nil
}

func (p *Plugin) rememberBinding(conversationID, sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bound[conversationID] = sessionID
}

func (p *Plugin) conversationsFor(sessionID string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var out []string
	for conversation, bound := range p.bound {
		if bound == sessionID && conversation != sessionID {
			out = append(out, conversation)
		}
	}
	return out
}

func wireParams(env v1.Envelope) v1.TransportInboundParams {
	params := v1.TransportInboundParams{
		Transport:      env.Transport,
		ConversationID: env.ConversationID,
		Principal:      env.Principal,
		SourceID:       env.SourceID,
		Kind:           string(env.Kind),
	}

	switch env.Kind {
	case v1.EnvelopeMessage:
		if env.Message != nil {
			params.Text = env.Message.Text
		}
	case v1.EnvelopeCommand:
		if env.Command != nil {
			params.Command = env.Command.Name
			params.Method = env.Command.Method
			params.Args = env.Command.Args
			params.ConfigID = env.Command.ConfigID
		}
	case v1.EnvelopeInteraction:
		if env.Interaction != nil {
			params.Action = env.Interaction.Action
			params.Value = env.Interaction.Value
		}
	}
	return params
}
