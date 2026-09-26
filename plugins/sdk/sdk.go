// Package sdk is the Go helper for writing Hive plugins.
//
// A plugin is a separate process that speaks the Hive Protocol over stdio. This
// package performs the handshake, dispatches core-invoked methods, and delivers
// subscribed events.
//
// It must not import any internal Hive package: a plugin depends on the public
// protocol surface, not on the core.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// HandshakeTimeout bounds how long the core gets to accept a plugin.
const HandshakeTimeout = 5 * time.Second

// DefaultEventBuffer is the delivered event queue depth.
const DefaultEventBuffer = 256

// Options describes the plugin to the core.
type Options struct {
	// ID is the stable plugin identity. It survives restarts.
	ID string

	Type    v1.PluginType
	Version string

	// Capabilities are the capabilities the plugin wants. The core grants a
	// subset; the plugin cannot grant itself authority by asking.
	Capabilities []string
}

// Handler serves a core-invoked method.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Host is a plugin's connection to the Hive core.
type Host struct {
	opts  Options
	ready v1.HelloResponse

	peer *v1.Peer

	mu           sync.Mutex
	handlers     map[string]Handler
	shutdownOnce sync.Once

	events  chan v1.DeliveredEvent
	dropped atomic.Int64

	// shutdown is closed when the core asks the plugin to stop.
	//
	// The core closes the connection at the same time, so noticing this is not
	// required to exit — but it is what gives a plugin the chance to end what it
	// started before the process goes away.
	shutdown chan struct{}
}

// Stdio returns a stream over the process's standard input and output.
func Stdio() *v1.Stream { return stdioStream(os.Stdin, os.Stdout) }

// stdioStream builds the stream a plugin talks to the core over.
//
// The standard streams are passed as closers because Close is how a plugin ends
// its read loop: Peer.Close closes the stream and then waits for the read loop
// to stop. A plugin that stops for its own reason — a transport whose
// connection dropped, say — returns from Run while the core is still connected,
// so nothing but closing the reader ends that read. Without it the process
// never exits and the supervisor, which restarts a plugin only on exit, never
// restarts it.
func stdioStream(r, w *os.File) *v1.Stream {
	return v1.NewStream(r, w, r, w)
}

// Connect performs the plugin handshake on stream.
func Connect(ctx context.Context, stream *v1.Stream, opts Options) (*Host, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}

	hello := v1.HelloRequest{
		PluginID:        opts.ID,
		Type:            opts.Type,
		Version:         opts.Version,
		ProtocolVersion: v1.Current(),
		Capabilities:    opts.Capabilities,
	}

	req, err := v1.NewRequest(v1.NumberID(1), v1.MethodPluginHello, hello)
	if err != nil {
		return nil, err
	}
	if err := stream.Write(req); err != nil {
		return nil, fmt.Errorf("sdk: send hello: %w", err)
	}

	resp, err := awaitResponse(ctx, stream, v1.NumberID(1))
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}

	var ready v1.HelloResponse
	if err := json.Unmarshal(resp.Result, &ready); err != nil {
		return nil, fmt.Errorf("sdk: decode ready: %w", err)
	}

	h := &Host{
		opts:     opts,
		ready:    ready,
		peer:     v1.NewPeer(stream),
		handlers: make(map[string]Handler),
		events:   make(chan v1.DeliveredEvent, DefaultEventBuffer),
		shutdown: make(chan struct{}),
	}
	h.peer.Start()
	return h, nil
}

// InstanceID returns the process instance identifier.
func (h *Host) InstanceID() string { return h.ready.InstanceID }

// ConnectionGeneration returns this connection's generation for the plugin
// identity.
func (h *Host) ConnectionGeneration() int64 { return h.ready.ConnectionGeneration }

// Capabilities returns the capabilities the core actually granted.
func (h *Host) Capabilities() []string { return h.ready.Capabilities }

// Granted reports whether the core granted a capability.
func (h *Host) Granted(capability string) bool {
	for _, c := range h.ready.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// Events streams delivered events. The channel is closed when the connection
// ends.
func (h *Host) Events() <-chan v1.DeliveredEvent { return h.events }

// DroppedEvents returns how many delivered events were dropped because the
// queue was full. Delivery is at-least-once, so an unacknowledged event is
// delivered again.
func (h *Host) DroppedEvents() int64 { return h.dropped.Load() }

// Done is closed when the connection ends.
func (h *Host) Done() <-chan struct{} { return h.peer.Done() }

// Handle registers the handler for a core-invoked method.
func (h *Host) Handle(method string, fn Handler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[method] = fn
}

// Call invokes a core method.
func (h *Host) Call(ctx context.Context, method string, params, result any) error {
	return h.peer.Call(ctx, method, params, result)
}

// CallAs invokes a core method while asserting a principal.
//
// Only a trusted transport may assert a principal, and only one that belongs to
// its own transport. The core validates the assertion, so naming a principal is
// never enough on its own.
func (h *Host) CallAs(ctx context.Context, actor, method string, params, result any) error {
	return h.peer.CallWithMeta(ctx, &v1.Meta{Actor: actor}, method, params, result)
}

// Subscribe asks the core to deliver events.
func (h *Host) Subscribe(ctx context.Context, req v1.SubscribeRequest) (v1.SubscribeResponse, error) {
	var resp v1.SubscribeResponse
	err := h.peer.Call(ctx, v1.MethodEventSubscribe, req, &resp)
	return resp, err
}

// Ack acknowledges that events up to sequence were processed.
func (h *Host) Ack(ctx context.Context, subscriptionID string, sequence int64) error {
	return h.AckFor(ctx, subscriptionID, sequence, "")
}

// AckFor acknowledges delivery for a specific conversation.
//
// Naming the conversation is what advances the durable binding cursor, so a
// restarted transport resumes from where it left off instead of replaying
// everything.
func (h *Host) AckFor(ctx context.Context, subscriptionID string, sequence int64, conversationID string) error {
	return h.peer.Call(ctx, v1.MethodEventAck, v1.AckRequest{
		SubscriptionID: subscriptionID,
		Sequence:       sequence,
		ConversationID: conversationID,
	}, nil)
}

// Run serves until ctx is cancelled or the connection ends.
//
// A connection that ends because the core went away is an ordinary shutdown, so
// it returns nil rather than an error.
func (h *Host) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-h.shutdown:
			return nil
		case <-h.peer.Done():
			return closedOr(h.peer.Err())
		case req, ok := <-h.peer.Requests():
			if !ok {
				return closedOr(h.peer.Err())
			}
			// A handler may run for minutes: an agent turn does. Handling requests
			// one at a time would block everything else until the turn ended, so a
			// cancel or a permission response could never be processed — and a turn
			// that asks for permission would wait for an answer that cannot arrive.
			go h.handleRequest(ctx, req)
		case note, ok := <-h.peer.Notifications():
			if !ok {
				return closedOr(h.peer.Err())
			}
			h.handleNotification(note)
		}
	}
}

// closedOr maps a closed connection to a nil error.
func closedOr(err error) error {
	if v1.IsClosed(err) {
		return nil
	}
	return err
}

// Close ends the connection.
func (h *Host) Close() error { return h.peer.Close() }

// signalShutdown ends the run loop once, however many times it is asked.
func (h *Host) signalShutdown() {
	h.shutdownOnce.Do(func() { close(h.shutdown) })
}

func (h *Host) handleRequest(ctx context.Context, req *v1.Message) {
	id := req.RequestID()

	// The core is stopping this plugin. The run loop is ended so the plugin's own
	// cleanup runs while the connection is still up, and the answer is sent first
	// so the core knows the request was seen.
	if req.Method == v1.MethodPluginShutdown {
		h.signalShutdown()
		_ = h.peer.Respond(id, map[string]any{"ok": true})
		return
	}

	h.mu.Lock()
	fn := h.handlers[req.Method]
	h.mu.Unlock()

	if fn == nil {
		_ = h.peer.RespondError(id, v1.MethodNotFound(req.Method))
		return
	}

	result, err := fn(ctx, req.Params)
	if err != nil {
		_ = h.peer.RespondError(id, v1.AsError(err))
		return
	}
	if err := h.peer.Respond(id, result); err != nil {
		return
	}
}

func (h *Host) handleNotification(note *v1.Message) {
	// The core asks a plugin to stop with a notification, not a request, so this
	// is where the run loop is ended. Handling it as a request too is harmless and
	// makes the intent obvious at both call sites.
	if note.Method == v1.MethodPluginShutdown {
		h.signalShutdown()
		return
	}
	if note.Method != v1.NotificationEvent {
		return
	}

	var delivered v1.DeliveredEvent
	if err := json.Unmarshal(note.Params, &delivered); err != nil {
		return
	}

	select {
	case h.events <- delivered:
	default:
		// Delivery is at-least-once and driven by the acknowledgement cursor,
		// so a dropped event is delivered again rather than lost.
		h.dropped.Add(1)
	}
}

// awaitResponse reads the next message and requires it to be the response to
// id. The handshake is the first exchange on a connection, so nothing else may
// arrive before it.
func awaitResponse(ctx context.Context, stream *v1.Stream, id v1.ID) (*v1.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()

	type result struct {
		msg *v1.Message
		err error
	}
	ch := make(chan result, 1)
	go func() {
		msg, err := stream.Read()
		ch <- result{msg, err}
	}()

	select {
	case <-ctx.Done():
		return nil, errors.New("sdk: handshake timed out")
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("sdk: handshake read: %w", r.err)
		}
		if r.msg.Kind() != v1.KindResponse || r.msg.RequestID() != id {
			return nil, fmt.Errorf("sdk: expected a response to %s, got a %s", id, r.msg.Kind())
		}
		return r.msg, nil
	}
}

func validateOptions(opts Options) error {
	switch {
	case opts.ID == "":
		return errors.New("sdk: plugin id is required")
	case !opts.Type.Valid():
		return fmt.Errorf("sdk: unknown plugin type %q", opts.Type)
	}
	return nil
}
