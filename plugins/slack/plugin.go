package slack

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

	"github.com/slack-go/slack/slackevents"
	"github.com/thuupx/hive/plugins/sdk"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
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

	// FlatReplies keeps everything in the channel instead of threading a turn's
	// output under the message that asked for it. A direct message is always flat.
	FlatReplies bool

	// TypingIndicator shows that a turn is working by animating one message, which
	// the answer then replaces. Off means a turn says nothing until it answers.
	TypingIndicator bool

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

// conversationKey is what a delivery is bound to, and where its output belongs.
//
// A thread is a conversation. Two threads in one channel are two conversations,
// and answering one must not answer in the other: binding a whole channel to one
// session mixes unrelated conversations and makes the thread an answer goes into a
// thing that has to be remembered and guessed at.
//
// Making the thread part of the key is what removes the guesswork. The thread a
// turn belongs to is then a property of its session, so nothing has to be tracked
// per run and nothing has to fall back to a previous value.
//
// A direct message is a private pipe with no threads to keep apart, so it stays
// flat and one conversation.
func conversationKey(channelID, messageTS, threadRoot string, flat bool) (key, thread string) {
	if threadRoot != "" {
		return channelID + ":" + threadRoot, threadRoot
	}
	if flat || messageTS == "" || isDirectMessage(channelID) {
		return channelID, ""
	}
	// A message in a channel starts a thread, and that thread is the conversation.
	return channelID + ":" + messageTS, messageTS
}

// threadOf is where a conversation's output belongs.
//
// It is read from the key rather than remembered, so a restart cannot lose it.
func threadOf(conversationID string) string {
	if i := strings.Index(conversationID, ":"); i >= 0 {
		return conversationID[i+1:]
	}
	return ""
}

// channelOf is the platform conversation a key belongs to.
func channelOf(conversationID string) string {
	if i := strings.Index(conversationID, ":"); i >= 0 {
		return conversationID[:i]
	}
	return conversationID
}

// isDirectMessage reports whether a conversation is 1:1.
//
// Slack ids say so: a direct message starts with D, a channel with C, and a group
// with G.
func isDirectMessage(conversationID string) bool {
	return strings.HasPrefix(conversationID, "D")
}

// delivery is one inbound platform delivery plus the coordinates needed to reply.
type delivery struct {
	envelope v1.Envelope

	// conversationID is what this delivery is bound to: the thread, not the
	// channel, because a thread is a conversation.
	conversationID string

	// channelID is the platform conversation, which is where a message is posted.
	channelID string

	timestamp string

	// messageTS is the message an interaction came from, so the card that
	// carried the button can be updated once it is pressed. Empty for a message.
	messageTS string

	// thread is where this turn's output belongs. Empty posts to the conversation
	// itself.
	thread string
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

	// toolRuns maps a turn to the one message that shows its tool calls, so a
	// turn is a message rather than one per call.
	//
	// It is bounded: a long-lived transport must not remember every turn it has
	// ever shown.
	toolRuns map[string]*toolRun

	// toolRunOrder is the order turns were shown in, so the oldest can be
	// forgotten.
	toolRunOrder []string

	// handled remembers recent deliveries, because one platform message can
	// arrive as more than one event.
	handled map[string]time.Time

	// lastSeen is the highest sequence delivered per session, so a gap can be
	// noticed and read back.
	lastSeen map[string]int64

	// cursors is how far each session has been accepted, read at startup so a
	// restart can replay what was missed.
	cursors map[string]int64

	// runThreads maps a run to the thread its output belongs in, so an answer
	// lands under the message that asked for it rather than in the channel.
	runThreads map[string]string

	// threadOrder is the order runs were threaded in, so the oldest can be
	// forgotten.
	threadOrder []string

	// typing is the message showing that a conversation's turn is working, keyed
	// by conversation because the run is not known when it is posted.
	typing map[string]*typing
}

// New returns a Slack plugin over a connected host.
func New(host *sdk.Host, client Client, opts Options) *Plugin {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Plugin{
		host:     host,
		client:   client,
		parser:   Parser{BotUserID: opts.BotUserID, RequireMention: opts.RequireMention},
		opts:     opts,
		log:      log,
		bound:    make(map[string]string),
		toolRuns: make(map[string]*toolRun),
		handled:  make(map[string]time.Time),
		cursors:  make(map[string]int64),
		typing:   make(map[string]*typing),
		lastSeen: make(map[string]int64),
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

	// The typing indicator is animated for as long as the transport runs.
	go p.tickTyping(ctx)

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

	// The socket's own "connected" line comes from the client, when the library
	// has actually connected. Saying it here would say it before dialling, and a
	// transport that reports itself connected without a socket is the state that
	// took three days to notice once.
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
		// Not addressed to Hive, or not something a user typed. Saying so at debug
		// level is what makes "I typed something and nothing happened" answerable.
		p.log.Debug("ignoring a delivery", "type", inbound.Type())
		return
	}

	// Every delivery that reaches Hive is logged before it is handled, so a
	// delivery that produces nothing is still visible. Without this the case a
	// user complains about — no answer at all — leaves no trace.
	p.log.Info("delivery received",
		"conversation", d.conversationID,
		"kind", string(d.envelope.Kind),
		"command", commandName(d.envelope),
		"threaded", d.thread != "",
	)

	// Slack describes one message with both an app_mention and a message event
	// when it mentions the bot, so the same delivery arrives twice. Hive would
	// deduplicate the command, but the user would see two acknowledgements.
	if p.alreadyHandled(d.envelope.SourceID) {
		p.log.Info("ignoring a repeat delivery", "source", d.envelope.SourceID)
		return
	}

	// The catalog is the transport's own, so help is answered without asking the
	// core. Sending it through Hive would mean the transport does not know what it
	// exposes.
	if d.envelope.Command != nil && d.envelope.Command.Name == HelpCommand {
		p.post(ctx, d.conversationID, d.thread, HelpMessage())
		return
	}

	p.fetchAttachments(ctx, &d.envelope)

	// Acknowledge as early as the transport can safely confirm acceptance, and
	// before waiting for the agent. The reaction is best-effort: failing to add
	// it must not fail the operation.
	p.acknowledge(ctx, d)

	// The turn's thread is recorded before the call, because the agent may start
	// working — and emitting tool calls — before the call returns.
	// A turn shows that it is working before the work starts, for the same reason:
	// the agent can emit its first tool call before the delivery returns, and the
	// indicator has to come first in the conversation.
	//
	// It is one message, and the answer replaces it, so a turn does not leave an
	// acknowledgement behind to say what the answer already says.
	working := d.envelope.Kind == v1.EnvelopeMessage
	if working {
		p.startTyping(ctx, d.conversationID, d.thread)
	}

	var outcome v1.TransportOutcome
	err := p.host.Call(ctx, v1.MethodTransportInbound, wireParams(d.envelope), &outcome)
	if err != nil {
		p.log.Warn("inbound delivery failed", "error", err)

		// The indicator goes rather than being left behind: a clock that keeps
		// ticking says the turn is still working, and it is not. Saying what
		// happened is the honest end to it.
		p.stopTyping(ctx, d.conversationID)
		p.post(ctx, d.conversationID, d.thread, textMessage(fmt.Sprintf(":warning: %s", err.Error())))
		return
	}

	if outcome.SessionID != "" {
		p.rememberBinding(d.conversationID, outcome.SessionID)
	}
	// The outcome is logged whatever it is, so a refused command is as visible as
	// a successful one.
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

	// A button press is answered on the card that carried the button. A second
	// message would leave the buttons where they were, and a card that still
	// offers Allow after the answer was given invites a second click that is
	// refused — which reads as the first one having failed.
	if d.envelope.Kind == v1.EnvelopeInteraction && outcome.Error == nil {
		p.settleInteraction(ctx, d)
		return
	}

	// The turn runs in the background, so there is nothing to say yet. The
	// reaction on the message is the acknowledgement, and the answer is the
	// response: a message that only says "working on it" is noise between the two.
	//
	// A failure is different. The turn is not running, so silence would be a lie.
	if working && outcome.Method == v1.MethodSessionPrompt && outcome.Error == nil {
		// The indicator, if one is showing, keeps ticking until the answer replaces
		// it. Nothing else is posted.
		return
	}

	// The outcome is its own message. The indicator goes when this delivery is the
	// turn the indicator belongs to — the message that started it, or the cancel
	// that ended it — and stays while a turn is still working.
	//
	// A status answer that took the "working" signal away would read as an agent
	// that had stopped, and the turn would still be running.
	if endsTurn(d, outcome) {
		p.stopTyping(ctx, d.conversationID)
	}
	p.post(ctx, d.conversationID, d.thread, RenderOutcome(outcome))
}

// endsTurn reports whether a delivery's outcome ends the turn its indicator
// stood for.
//
// A message is the turn: its answer ends it, and a failure means there was no
// turn, so the indicator must go too. A cancel ends the turn it was shown for.
// Everything else — a status read, a settings change — leaves the turn working.
func endsTurn(d delivery, outcome v1.TransportOutcome) bool {
	if d.envelope.Kind == v1.EnvelopeMessage {
		return true
	}
	return outcome.Error == nil && outcome.Method == v1.MethodSessionCancel
}

// settleInteraction replaces a card's buttons with the decision that was made.
//
// The card is the message the button was on, which Slack names in the action.
// The buttons go with it: leaving them invites a second press, and the second
// press is refused because the request is no longer pending.
func (p *Plugin) settleInteraction(ctx context.Context, d delivery) {
	if d.messageTS == "" {
		return
	}
	if err := p.client.UpdateMessage(ctx, UpdateMessageRequest{
		Channel:   channelOf(d.conversationID),
		Timestamp: d.messageTS,
		Message:   resolvedMessage(d.envelope),
	}); err != nil {
		p.log.Warn("could not settle a permission card",
			"conversation", d.conversationID, "error", err)
	}
}

// resolvedMessage is what a card says once its button was pressed.
func resolvedMessage(env v1.Envelope) Message {
	if label := chosenLabel(env); label != "" {
		return textMessage(":white_check_mark: " + label)
	}
	return textMessage(":white_check_mark: Resolved")
}

// chosenLabel reads the label the transport put on the button that was pressed.
//
// The label is the agent's own wording for the choice, kept with the button so
// the card can say what was chosen without asking the core again.
func chosenLabel(env v1.Envelope) string {
	if env.Interaction == nil {
		return ""
	}
	var value struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal([]byte(env.Interaction.Value), &value); err != nil {
		return ""
	}
	return value.Label
}

// messageEvent is the message an Events API delivery carried.
//
// Only a callback event carries one: a url_verification or a rate-limit notice
// is not user intent, and neither is a mention handled as its own event type —
// the message event carries the mention text, which is what addressing is read
// from.
func messageEvent(event slackevents.EventsAPIEvent) (slackevents.MessageEvent, bool) {
	if event.Type != slackevents.CallbackEvent {
		return slackevents.MessageEvent{}, false
	}
	message, ok := event.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok || message == nil {
		return slackevents.MessageEvent{}, false
	}
	return *message, true
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
	switch {
	case inbound.Interaction != nil:
		payload := *inbound.Interaction
		env := p.parser.ParseInteraction(payload)
		// The conversation is the thread the card is in, which is what the thread
		// root says. A card that is not in a thread belongs to the channel.
		root := payload.Message.ThreadTimestamp
		key, thread := conversationKey(env.ConversationID, root, root, p.opts.FlatReplies)
		env.ConversationID = key
		return delivery{
			envelope:       env,
			conversationID: key,
			channelID:      env.ConversationID,
			timestamp:      payload.ActionTs,
			messageTS:      payload.Message.Timestamp,
			thread:         thread,
		}, true

	case inbound.Events != nil:
		event, ok := messageEvent(*inbound.Events)
		if !ok {
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

		env := p.parser.ParseMessage(event, messageFiles(inbound.Payload))
		if env.Kind == "" {
			// Not addressed to Hive.
			return delivery{}, false
		}
		key, thread := conversationKey(event.Channel, event.TimeStamp, event.ThreadTimeStamp, p.opts.FlatReplies)
		// Hive binds what the envelope names, so the envelope names the conversation:
		// the thread, not the channel that holds it. Binding the channel would make
		// two threads one conversation again.
		env.ConversationID = key
		return delivery{
			envelope:       env,
			conversationID: key,
			channelID:      event.Channel,
			timestamp:      event.TimeStamp,
			thread:         thread,
		}, true
	}

	return delivery{}, false
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

			// A gap means events were published while this transport was not
			// reading — the subscription is set up after the plugin starts, and a
			// burst can overflow the bus. The cursor is what recovers them, so the
			// missing range is read from the durable store rather than skipped:
			// an answer that never arrives is what a user reports as a bot that
			// ignored them.
			p.recoverGap(ctx, delivered.Event.SessionID, delivered.Event.Sequence)

			// Acknowledge delivery so the core can advance the cursor. Delivery
			// is at-least-once, so an unacknowledged event arrives again.
			conversations := p.conversationsFor(delivered)

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

			// The answer belongs under the message that asked for it, so a channel
			// stays readable and a turn's output stays together. The thread is read
			// from the conversation the event is bound to, so it cannot be guessed
			// wrong and cannot be lost by a restart.
			thread := ""
			for _, conversation := range conversations {
				if thread = threadOf(conversation); thread != "" {
					break
				}
			}

			renderer := Renderer{SessionID: delivered.Event.SessionID}
			rendered, ok := renderer.RenderEvent(delivered.Event)

			// The answer is a message of its own, and the indicator that said the
			// turn was working goes away with it.
			//
			// It is not edited into the indicator's place: that would put the
			// answer above the turn's own tool list, out of order, and Slack
			// refuses an edited text over 4,000 characters while a posted one may
			// be far larger. A long answer that could never be posted that way is
			// exactly what a user reports as a bot that stopped answering.
			if ok && isAnswer(delivered.Event) {
				for _, conversation := range conversations {
					p.stopTyping(ctx, conversation)
				}
			}

			if ok {
				p.log.Info("rendering an event",
					"session", delivered.Event.SessionID,
					"sequence", delivered.Event.Sequence,
					"type", delivered.Event.Type,
					"conversations", len(conversations),
					"threaded", thread != "",
				)
				for _, conversation := range conversations {
					if rendered.ToolCall != nil {
						p.renderToolRun(ctx, conversation, thread, rendered)
						continue
					}
					p.post(ctx, conversation, thread, rendered.Message)
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

// toolRun is one turn's tool calls and the message that shows them.
type toolRun struct {
	// messageTS is the message the turn's calls are listed in, once it is posted.
	messageTS string

	// calls are the turn's tool calls in the order they were first seen, so the
	// list does not reorder under the reader as calls are updated.
	calls []v1.ToolCall

	// index finds a call's place in calls by the agent's own identifier.
	index map[string]int

	// detailed records the calls whose output was already posted in the thread,
	// so an update does not post it again.
	detailed map[string]bool
}

// upsert records a tool call's latest state, keeping its first position.
func (r *toolRun) upsert(call v1.ToolCall) {
	if at, ok := r.index[call.ToolCallID]; ok {
		r.calls[at] = call
		return
	}
	r.index[call.ToolCallID] = len(r.calls)
	r.calls = append(r.calls, call)
}

// renderToolRun keeps one message per turn for the tool calls it made.
//
// A turn can make many calls and each call produces many updates, so one message
// per call buries the conversation. One message per turn keeps it readable, and
// Slack collapses the list itself once it is long.
func (p *Plugin) renderToolRun(ctx context.Context, conversation, thread string, rendered Rendered) {
	runID := rendered.ToolRunID
	if runID == "" {
		runID = rendered.ToolCall.ToolCallID
	}

	p.mu.Lock()
	run := p.rememberToolRunLocked(runID)
	run.upsert(*rendered.ToolCall)
	message := toolRunMessage(run.calls)
	timestamp := run.messageTS
	p.mu.Unlock()

	if timestamp == "" {
		ts, err := p.client.PostMessage(ctx, PostMessageRequest{
			Channel:  channelOf(conversation),
			ThreadTS: thread,
			Message:  message,
		})
		if err != nil {
			p.log.Warn("could not post a turn's tool calls", "conversation", conversation, "error", err)
			return
		}

		p.mu.Lock()
		if current := p.toolRuns[runID]; current != nil {
			current.messageTS = ts
		}
		p.mu.Unlock()
		timestamp = ts
	} else if err := p.client.UpdateMessage(ctx, UpdateMessageRequest{
		Channel:   channelOf(conversation),
		Timestamp: timestamp,
		Message:   message,
	}); err != nil {
		p.log.Warn("could not update a turn's tool calls", "conversation", conversation, "error", err)
	}

	// A failed call's output is worth reading in full, so it goes in the thread
	// under the list rather than in it, once per call.
	if rendered.Detail == "" {
		return
	}
	p.mu.Lock()
	already := p.toolRuns[runID] != nil && p.toolRuns[runID].detailed[rendered.ToolCall.ToolCallID]
	if current := p.toolRuns[runID]; current != nil {
		current.detailed[rendered.ToolCall.ToolCallID] = true
	}
	p.mu.Unlock()
	if already {
		return
	}
	p.post(ctx, conversation, timestamp, textMessage(rendered.Detail))
}

// rememberToolRunLocked returns the run for a turn, creating it and forgetting
// the oldest when the bound is reached. The caller holds the lock.
func (p *Plugin) rememberToolRunLocked(runID string) *toolRun {
	if run := p.toolRuns[runID]; run != nil {
		return run
	}

	run := &toolRun{index: make(map[string]int), detailed: make(map[string]bool)}
	p.toolRuns[runID] = run
	p.toolRunOrder = append(p.toolRunOrder, runID)
	for len(p.toolRunOrder) > maxToolRuns {
		oldest := p.toolRunOrder[0]
		p.toolRunOrder = p.toolRunOrder[1:]
		delete(p.toolRuns, oldest)
	}
	return run
}

// maxRunThreads bounds how many runs are remembered.
const maxRunThreads = 1024

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

	req := ReactionRequest{Channel: d.channelID, Timestamp: d.timestamp, Name: reaction}
	if err := p.client.AddReaction(ctx, req); err != nil {
		p.log.Debug("acknowledgement failed", "error", err)
	}
}

func (p *Plugin) post(ctx context.Context, conversationID, threadTS string, message Message) {
	if conversationID == "" {
		return
	}
	// The conversation is the thread, and Slack posts to the channel that holds it.
	if _, err := p.client.PostMessage(ctx, PostMessageRequest{
		Channel:  channelOf(conversationID),
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
		conversations := p.boundConversations(sessionID)
		thread := ""
		for _, conversation := range conversations {
			if thread = threadOf(conversation); thread != "" {
				break
			}
		}
		for _, conversation := range conversations {
			if message.ToolCall != nil {
				p.renderToolRun(ctx, conversation, thread, message)
				continue
			}
			p.post(ctx, conversation, thread, message.Message)
		}
		rendered++
	}
	return rendered, nil
}

// catchUpLimit bounds how much history a restart replays.
const catchUpLimit = 200

// maxToolRuns bounds how many turns a transport remembers.
//
// A turn keeps one message, so this is one entry per turn the transport has
// shown. Forgetting the oldest means a very old turn's list is reposted rather
// than updated, which is a smaller problem than growing forever.
const maxToolRuns = 1024

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

// recoverGap replays what was missed between two delivered sequences.
//
// A subscription is not a guarantee: events published before it exists, or while
// the reader is behind, are gone from the bus. The durable store still has them,
// and the sequence says exactly which ones.
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
//
// It is zero when there is nothing to read back: the first event of a session, or
// one that follows the last.
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
//
// It is the local view, used where the conversation was already found locally.
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
