package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

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

	Log *slog.Logger
}

// Acknowledgement is the optional "Hive received this" signal.
//
// It means the message was received and recognized. It must never be read as
// agent started, working, or finished. Its failure cannot fail the operation.
type Acknowledgement struct {
	Enabled  bool
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
	toolMessages map[string]string
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
	}
}

// Run serves Slack until ctx is cancelled or the connection ends.
func (p *Plugin) Run(ctx context.Context) error {
	if err := p.reloadBindings(ctx); err != nil {
		p.log.Warn("could not load bound sessions", "error", err)
	}

	// Subscribe to every session this transport is authorized for, and filter
	// while rendering.
	subscription, err := p.host.Subscribe(ctx, v1.SubscribeRequest{})
	if err != nil {
		return fmt.Errorf("slack: subscribe to events: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go p.renderEvents(ctx, subscription.SubscriptionID)

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

		// A bot's own message, an edit, or a join is not user intent.
		if event.BotID != "" || event.SubType != "" || event.User == "" {
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

// renderEvents posts Hive events into the conversations that belong to them.
func (p *Plugin) renderEvents(ctx context.Context, subscriptionID string) {
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

			// Acknowledge delivery so the core can advance the cursor. Delivery
			// is at-least-once, so an unacknowledged event arrives again.
			conversations := p.conversationsFor(delivered.Event.SessionID)

			renderer := Renderer{SessionID: delivered.Event.SessionID}
			rendered, ok := renderer.RenderEvent(delivered.Event)
			if ok {
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

// acknowledge adds the optional "received" reaction.
//
// It is presentation feedback only, and its failure never fails the operation.
func (p *Plugin) acknowledge(ctx context.Context, d delivery) {
	if !p.opts.Acknowledgement.Enabled {
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
		}
	case v1.EnvelopeInteraction:
		if env.Interaction != nil {
			params.Action = env.Interaction.Action
			params.Value = env.Interaction.Value
		}
	}
	return params
}
