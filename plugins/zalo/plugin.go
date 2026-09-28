package zalo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/thuupx/hive/plugins/sdk"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// Options configures the Zalo plugin.
type Options struct {
	// BotName is the bot's display name, used to recognize addressing in a group.
	BotName string

	// RequireMention ignores a group message that does not address the bot. A
	// private chat is 1:1, so it is always addressed and this does not apply.
	RequireMention bool

	// Acknowledgement is the optional immediate feedback.
	Acknowledgement Acknowledgement

	// MaxAttachmentBytes bounds how large a file the transport will read. Zero
	// uses the default.
	MaxAttachmentBytes int64

	// TypingIndicator shows that a turn is working with Zalo's transient chat
	// action. Off means a turn says nothing until it answers.
	TypingIndicator bool

	Log *slog.Logger
}

// Acknowledgement is the optional "Hive received this" signal.
//
// It means the message was received and recognized. It must never be read as
// agent started, working, or finished. Its failure cannot fail the operation.
//
// Zalo has no reactions, so the signal is the transient typing action rather than
// a mark on the user's message.
type Acknowledgement struct {
	Enabled bool

	// Mode is accepted for configuration parity. Reaction has no Zalo
	// equivalent, so any enabled acknowledgement is shown as typing.
	Mode string

	Reaction string
}

// delivery is one inbound delivery plus the coordinates needed to reply.
type delivery struct {
	envelope v1.Envelope

	// conversationID is what this delivery is bound to: the chat.
	conversationID string

	// chatID is the platform conversation, which is where a message is posted.
	chatID string

	messageID string
}

// Plugin wires Zalo into a Hive transport plugin.
type Plugin struct {
	host   *sdk.Host
	client Client
	parser Parser
	opts   Options
	log    *slog.Logger

	mu    sync.Mutex
	bound map[string]string // conversation id to session id

	// handled remembers recent deliveries, because one platform message can
	// arrive more than once.
	handled map[string]time.Time

	// lastSeen is the highest sequence delivered per session, so a gap can be
	// noticed and read back.
	lastSeen map[string]int64

	// cursors is how far each session has been accepted, read at startup so a
	// restart can replay what was missed.
	cursors map[string]int64

	// pending is the permission request each conversation is waiting on, so a
	// reply in words can be turned into a decision. Zalo has no buttons.
	pending map[string]*permissionRequest

	// pendingOrder is the order requests were seen in, so the oldest can be
	// forgotten.
	pendingOrder []string

	// typing is the conversations showing the working signal.
	typing map[string]*typing
}

// New returns a Zalo plugin over a connected host.
func New(host *sdk.Host, client Client, opts Options) *Plugin {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Plugin{
		host:     host,
		client:   client,
		parser:   Parser{BotName: opts.BotName, RequireMention: opts.RequireMention},
		opts:     opts,
		log:      log,
		bound:    make(map[string]string),
		handled:  make(map[string]time.Time),
		cursors:  make(map[string]int64),
		lastSeen: make(map[string]int64),
		pending:  make(map[string]*permissionRequest),
		typing:   make(map[string]*typing),
	}
}

// Run serves Zalo until ctx is cancelled or the connection ends.
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
		return fmt.Errorf("zalo: subscribe to events: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Rendering talks to the platform, which is slow, and the delivery channel is
	// bounded. Draining it must never wait for a post, or a burst of agent events
	// overflows the buffer and the events are lost.
	render := make(chan v1.DeliveredEvent, renderQueue)
	go p.renderWorker(ctx, subscription.SubscriptionID, render)
	go p.drain(ctx, render)

	// The working signal is refreshed for as long as the transport runs.
	go p.tickTyping(ctx)

	// The SDK loop dispatches core-invoked methods and delivers subscribed
	// events. Either loop ending ends the plugin, so a dead connection is not a
	// silent half-running transport.
	ended := make(chan error, 2)
	go func() { ended <- p.host.Run(ctx) }()
	go func() { ended <- p.serveInbound(ctx) }()

	err = <-ended
	cancel()
	return err
}

// dispatch hands a delivered event to the render worker.
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

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.host.Done():
			return nil
		case inbound, ok := <-events:
			if !ok {
				return errors.New("zalo: the polling connection ended")
			}
			p.handleInbound(ctx, inbound)
		}
	}
}

func (p *Plugin) handleInbound(ctx context.Context, inbound Update) {
	d, ok := p.parse(inbound)
	if !ok {
		// Not addressed to Hive, or not something a user typed. Saying so at
		// debug level is what makes "I typed something and nothing happened"
		// answerable.
		p.log.Debug("ignoring a delivery", "type", inbound.EventName)
		return
	}

	// Every delivery that reaches Hive is logged before it is handled, so a
	// delivery that produces nothing is still visible.
	p.log.Info("delivery received",
		"conversation", d.conversationID,
		"kind", string(d.envelope.Kind),
		"command", commandName(d.envelope),
	)

	if p.alreadyHandled(d.envelope.SourceID) {
		p.log.Info("ignoring a repeat delivery", "source", d.envelope.SourceID)
		return
	}

	// The catalog is the transport's own, so help is answered without asking the
	// core.
	if d.envelope.Command != nil && d.envelope.Command.Name == HelpCommand {
		p.post(ctx, d.conversationID, HelpMessage())
		return
	}

	// Zalo has no buttons, so a permission is answered in words. A reply that
	// reads as a decision becomes an interaction, which is what the router
	// performs a permission response with.
	if d.envelope.Kind == v1.EnvelopeMessage {
		if pending := p.pendingFor(d.conversationID); pending != nil {
			if optionID, approved, ok := permissionDecision(d.envelope.Message.Text, pending); ok {
				p.clearPending(d.conversationID)
				d.envelope = permissionEnvelope(d, pending, optionID, approved)
			}
		}
	}

	p.fetchAttachments(ctx, &d.envelope)

	// A turn shows that it is working before the work starts: the agent can emit
	// its first event before the delivery returns, and the signal has to come
	// first in the conversation.
	working := d.envelope.Kind == v1.EnvelopeMessage
	if working {
		p.startTyping(ctx, d.conversationID)
	} else {
		// A command answers immediately, so the acknowledgement is the whole
		// signal rather than a status that would still be ticking when the
		// answer arrives.
		p.acknowledge(ctx, d)
	}

	var outcome v1.TransportOutcome
	err := p.host.Call(ctx, v1.MethodTransportInbound, wireParams(d.envelope), &outcome)
	if err != nil {
		p.log.Warn("inbound delivery failed", "error", err)

		// The signal goes rather than being left behind: it would say the turn
		// is still working, and it is not.
		p.stopTyping(d.conversationID)
		p.post(ctx, d.conversationID, textMessage(fmt.Sprintf("⚠️ %s", err.Error())))
		return
	}

	if outcome.SessionID != "" {
		p.rememberBinding(d.conversationID, outcome.SessionID)
	}
	if outcome.Error != nil {
		p.log.Warn("the delivery was refused",
			"conversation", d.conversationID,
			"method", outcome.Method,
			"error", outcome.Error.Message,
		)
	} else {
		p.log.Info("delivery handled",
			"conversation", d.conversationID,
			"method", outcome.Method,
			"session", outcome.SessionID,
		)
	}

	// A permission answer is its own delivery: the resolved event says so, and a
	// second message would only repeat it.
	if d.envelope.Kind == v1.EnvelopeInteraction && outcome.Error == nil {
		return
	}

	// The turn runs in the background, so there is nothing to say yet. The
	// working signal is the acknowledgement, and the answer is the response.
	if working && outcome.Method == v1.MethodSessionPrompt && outcome.Error == nil {
		return
	}

	if endsTurn(d, outcome) {
		p.stopTyping(d.conversationID)
	}
	p.post(ctx, d.conversationID, RenderOutcome(outcome))
}

// endsTurn reports whether a delivery's outcome ends the turn its signal stood
// for.
func endsTurn(d delivery, outcome v1.TransportOutcome) bool {
	if d.envelope.Kind == v1.EnvelopeMessage {
		return true
	}
	return outcome.Error == nil && outcome.Method == v1.MethodSessionCancel
}

// parse turns a platform delivery into an envelope, reporting false when the
// delivery is not for Hive.
func (p *Plugin) parse(inbound Update) (delivery, bool) {
	env, ok := p.parser.ParseUpdate(inbound)
	if !ok {
		return delivery{}, false
	}

	d := delivery{
		envelope:       env,
		conversationID: env.ConversationID,
		chatID:         env.ConversationID,
	}
	if inbound.Message != nil {
		d.messageID = inbound.Message.MessageID
	}
	return d, true
}

// permissionEnvelope turns a text reply into a permission response.
func permissionEnvelope(d delivery, pending *permissionRequest, optionID string, approved bool) v1.Envelope {
	value, err := json.Marshal(map[string]any{
		"agentRequestId": pending.AgentRequestID,
		"approved":       approved,
		"optionId":       optionID,
	})
	if err != nil {
		value = []byte(`{}`)
	}

	env := d.envelope
	env.Kind = v1.EnvelopeInteraction
	env.Message = nil
	env.Command = nil
	env.Interaction = &v1.IncomingInteraction{
		Action: actionFor(approved),
		Method: v1.MethodPermissionRespond,
		Value:  string(value),
	}
	return env
}

// Permission actions.
const (
	ActionPermissionAllow = "permission_allow"
	ActionPermissionDeny  = "permission_deny"
)

func actionFor(approved bool) string {
	if approved {
		return ActionPermissionAllow
	}
	return ActionPermissionDeny
}

// commandName is the command a delivery carried, if it carried one.
func commandName(env v1.Envelope) string {
	if env.Command == nil {
		return ""
	}
	return env.Command.Name
}

// fetchAttachments reads the files a user sent.
//
// Reading the platform is the transport's job, so the bytes are fetched here and
// only the bytes travel onward. A file that cannot be read is dropped with a note
// rather than failing the message.
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

// renderQueue bounds how much rendering may be outstanding.
const renderQueue = 2048

// renderWorker posts Hive events into the conversations that belong to them.
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

			// A gap means events were published while this transport was not
			// reading. The cursor is what recovers them.
			p.recoverGap(ctx, delivered.Event.SessionID, delivered.Event.Sequence)

			conversations := p.conversationsFor(delivered)
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

			// The answer is its own message, and the working signal goes away
			// with it.
			if ok && isAnswer(delivered.Event) {
				for _, conversation := range conversations {
					p.stopTyping(conversation)
				}
			}

			if ok {
				p.log.Info("rendering an event",
					"session", delivered.Event.SessionID,
					"sequence", delivered.Event.Sequence,
					"type", delivered.Event.Type,
					"conversations", len(conversations),
				)
				for _, conversation := range conversations {
					if rendered.Permission != nil {
						p.rememberPending(conversation, *rendered.Permission)
					}
					p.post(ctx, conversation, rendered.Message)
				}
			}

			// Acknowledge once per conversation so each durable binding cursor
			// advances.
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

// maxPending bounds how many permission requests are remembered.
const maxPending = 256

// rememberPending records the request a conversation is waiting on.
func (p *Plugin) rememberPending(conversationID string, request permissionRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, known := p.pending[conversationID]; !known {
		p.pendingOrder = append(p.pendingOrder, conversationID)
	}
	p.pending[conversationID] = &request

	for len(p.pendingOrder) > maxPending {
		oldest := p.pendingOrder[0]
		p.pendingOrder = p.pendingOrder[1:]
		delete(p.pending, oldest)
	}
}

// pendingFor is the request a conversation is waiting on, if any.
func (p *Plugin) pendingFor(conversationID string) *permissionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending[conversationID]
}

// clearPending forgets a request that has been answered.
func (p *Plugin) clearPending(conversationID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pending, conversationID)
}

// alreadyHandled reports whether a delivery was seen recently.
func (p *Plugin) alreadyHandled(sourceID string) bool {
	if sourceID == "" {
		return false
	}

	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()

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

// acknowledge sends the optional "received" signal.
//
// It is presentation feedback only, and its failure never fails the operation.
func (p *Plugin) acknowledge(ctx context.Context, d delivery) {
	if !p.opts.Acknowledgement.Enabled || p.opts.Acknowledgement.Mode == "none" {
		return
	}
	if err := p.client.SendChatAction(ctx, d.chatID, ActionTyping); err != nil {
		p.log.Debug("acknowledgement failed", "error", err)
	}
}

// post sends a message, split so it fits Zalo's limit.
//
// The text goes first and its images follow as photos: Zalo draws no picture
// inside a text message.
func (p *Plugin) post(ctx context.Context, conversationID string, message Message) {
	if conversationID == "" || (message.Text == "" && len(message.Images) == 0) {
		return
	}
	for _, part := range chunk(message.Text, ChunkChars) {
		if part == "" {
			continue
		}
		if _, err := p.client.SendMessage(ctx, SendMessageRequest{
			ChatID:  conversationID,
			Message: Message{Text: part, ParseMode: message.ParseMode},
		}); err != nil {
			p.log.Warn("could not post to Zalo", "conversation", conversationID, "error", err)
			return
		}
	}
	for _, image := range message.Images {
		p.sendImage(ctx, conversationID, image)
	}
}

// sendImage posts one picture.
//
// The Zalo Bot API fetches a photo from an http(s) URL and accepts no upload,
// so a file path cannot become a photo: it is posted as a text reference, as
// is a picture whose send fails, rather than being silently dropped.
func (p *Plugin) sendImage(ctx context.Context, chatID string, image OutboundImage) {
	if !strings.HasPrefix(image.Target, "http://") && !strings.HasPrefix(image.Target, "https://") {
		p.postImageFallback(ctx, chatID, image)
		return
	}

	if _, err := p.client.SendPhoto(ctx, SendPhotoRequest{
		ChatID:  chatID,
		Caption: caption(image.Alt),
		URL:     image.Target,
	}); err != nil {
		p.log.Warn("could not send an image", "target", image.Target, "error", err)
		p.postImageFallback(ctx, chatID, image)
	}
}

// postImageFallback keeps the reference visible when its picture cannot go: a
// path the user can open beats a silently dropped image.
func (p *Plugin) postImageFallback(ctx context.Context, chatID string, image OutboundImage) {
	label := image.Alt
	if label == "" {
		label = "image"
	}
	p.post(ctx, chatID, plainMessage(fmt.Sprintf("🖼 %s: %s", label, image.Target)))
}

// caption fits the alt text into what a photo caption may hold.
func caption(alt string) string {
	if runes := []rune(alt); len(runes) > CaptionChars {
		return string(runes[:CaptionChars])
	}
	return alt
}

// catchUp renders what was published while this transport was not running.
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
		conversations := p.boundConversations(sessionID)
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
		for _, conversation := range p.boundConversations(sessionID) {
			if message.Permission != nil {
				p.rememberPending(conversation, *message.Permission)
			}
			p.post(ctx, conversation, message.Message)
		}
		rendered++
	}
	return rendered, nil
}

// catchUpLimit bounds how much history a restart replays.
const catchUpLimit = 200

// reloadBindings rebuilds the conversation-to-session map.
//
// The coordinator owns the bindings, so the transport asks which sessions it can
// reach rather than remembering them across a restart.
func (p *Plugin) reloadBindings(ctx context.Context) error {
	var list v1.SessionListResult
	if err := p.host.Call(ctx, v1.MethodSessionList, v1.SessionListParams{}, &list); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, session := range list.Sessions {
		if _, known := p.bound[session.SessionID]; !known {
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

// recoverGap replays what was missed between two delivered sequences.
func (p *Plugin) recoverGap(ctx context.Context, sessionID string, sequence int64) {
	if sessionID == "" || sequence <= 1 {
		return
	}

	after := p.gapFrom(sessionID, sequence)
	if after == 0 {
		return
	}

	p.log.Warn("events were missed; reading them back",
		"session", sessionID, "after", after, "next", sequence)

	if _, err := p.replay(ctx, sessionID, after); err != nil {
		p.log.Warn("the missed events could not be read back",
			"session", sessionID, "error", err)
	}
}

// gapFrom records the sequence and reports where to resume from.
func (p *Plugin) gapFrom(sessionID string, sequence int64) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	last, seen := p.lastSeen[sessionID]
	if !seen {
		p.lastSeen[sessionID] = sequence
		return 0
	}
	if sequence <= last {
		return 0
	}

	p.lastSeen[sessionID] = sequence
	if sequence == last+1 {
		return 0
	}
	return last
}

// boundConversations is what this transport has learned about a session.
func (p *Plugin) boundConversations(sessionID string) []string {
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

// conversationsFor is where an event belongs.
//
// The core owns the binding and says which conversations a session has, so a
// restart does not lose them. What this transport learned from its own deliveries
// is added, because a binding made moments ago is already true here.
func (p *Plugin) conversationsFor(delivered v1.DeliveredEvent) []string {
	sessionID := delivered.Event.SessionID

	p.mu.Lock()
	defer p.mu.Unlock()

	seen := map[string]bool{}
	out := make([]string, 0, len(delivered.Conversations))
	for _, conversation := range delivered.Conversations {
		if conversation == "" || seen[conversation] {
			continue
		}
		seen[conversation] = true
		out = append(out, conversation)
	}

	for conversation, bound := range p.bound {
		if bound != sessionID || conversation == sessionID || seen[conversation] {
			continue
		}
		seen[conversation] = true
		out = append(out, conversation)
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
			params.Attachments = env.Message.Attachments
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
			params.Method = env.Interaction.Method
			params.Value = env.Interaction.Value
		}
	}
	return params
}
