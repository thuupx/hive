package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
)

// Peer correlates JSON-RPC requests and responses over a Stream.
//
// It is the shared request/response machinery for any Hive protocol
// connection: a plugin link, a node link, or a local client. Keeping it in the
// protocol package means a plugin can use it without linking the core.
//
// A Peer is safe for concurrent use. Inbound requests and notifications are
// handed to the caller through channels so the read loop never blocks on
// application work.
type Peer struct {
	stream  *Stream
	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan *Message
	closed  bool
	failed  bool
	err     error

	requests      chan *Message
	notifications chan *Message
	dropped       atomic.Int64

	done chan struct{}
	wg   sync.WaitGroup
}

// DefaultInboundBuffer is the inbound queue depth for requests and
// notifications.
const DefaultInboundBuffer = 256

// NewPeer returns a peer over stream. Call Start to begin reading.
func NewPeer(stream *Stream) *Peer {
	return &Peer{
		stream:        stream,
		pending:       make(map[uint64]chan *Message),
		requests:      make(chan *Message, DefaultInboundBuffer),
		notifications: make(chan *Message, DefaultInboundBuffer),
		done:          make(chan struct{}),
	}
}

// Start begins reading from the stream.
func (p *Peer) Start() {
	p.wg.Add(1)
	go p.readLoop()
}

// Requests streams inbound requests. The channel is closed when the connection
// ends.
func (p *Peer) Requests() <-chan *Message { return p.requests }

// Notifications streams inbound notifications. The channel is closed when the
// connection ends.
func (p *Peer) Notifications() <-chan *Message { return p.notifications }

// DroppedNotifications returns how many inbound notifications were dropped
// because the queue was full.
func (p *Peer) DroppedNotifications() int64 { return p.dropped.Load() }

// Done is closed when the connection ends.
func (p *Peer) Done() <-chan struct{} { return p.done }

// Err returns the error that ended the connection, if any.
func (p *Peer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Call sends a request and waits for its response.
func (p *Peer) Call(ctx context.Context, method string, params, result any) error {
	id := p.allocID()
	ch := make(chan *Message, 1)

	p.mu.Lock()
	if p.closed || p.failed {
		p.mu.Unlock()
		return ErrPeerClosed
	}
	p.pending[id] = ch
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()

	req, err := NewRequest(NumberID(id), method, params)
	if err != nil {
		return err
	}
	if err := p.write(req); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case reply, ok := <-ch:
		if !ok {
			return p.Err()
		}
		if reply.Error != nil {
			return reply.Error
		}
		if result != nil && len(reply.Result) > 0 {
			if err := json.Unmarshal(reply.Result, result); err != nil {
				return fmt.Errorf("hive: decode %s result: %w", method, err)
			}
		}
		return nil
	}
}

// Notify sends a notification. It does not wait for a response.
func (p *Peer) Notify(method string, params any) error {
	msg, err := NewNotification(method, params)
	if err != nil {
		return err
	}
	return p.write(msg)
}

// Respond answers an inbound request.
func (p *Peer) Respond(id ID, result any) error {
	msg, err := NewResult(id, result)
	if err != nil {
		return err
	}
	return p.write(msg)
}

// RespondError answers an inbound request with a protocol error.
func (p *Peer) RespondError(id ID, e *Error) error {
	return p.write(NewErrorResponse(id, e))
}

// Close ends the connection and releases every waiting call.
func (p *Peer) Close() error {
	p.mu.Lock()
	alreadyClosed := p.closed
	p.closed = true
	p.mu.Unlock()

	if alreadyClosed {
		return nil
	}
	err := p.stream.Close()
	p.fail(ErrPeerClosed)
	p.wg.Wait()
	return err
}

func (p *Peer) readLoop() {
	defer p.wg.Done()
	defer close(p.requests)
	defer close(p.notifications)

	for {
		msg, err := p.stream.Read()
		if err != nil {
			p.fail(fmt.Errorf("hive: read: %w", err))
			return
		}
		p.dispatch(msg)
	}
}

func (p *Peer) dispatch(msg *Message) {
	switch msg.Kind() {
	case KindRequest:
		select {
		case p.requests <- msg:
		default:
			// The application is not draining requests. Refusing is better
			// than silently dropping a request that expects an answer.
			_ = p.RespondError(msg.RequestID(), Unavailable("request queue is full"))
		}
	case KindNotification:
		select {
		case p.notifications <- msg:
		default:
			p.dropped.Add(1)
		}
	case KindResponse:
		id, ok := msg.RequestID().Number()
		if !ok {
			return
		}
		p.mu.Lock()
		ch, found := p.pending[id]
		if found {
			delete(p.pending, id)
		}
		p.mu.Unlock()
		if found {
			// Buffered, so a response never blocks the read loop.
			ch <- msg
		}
	}
}

func (p *Peer) write(msg *Message) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	unusable := p.closed || p.failed
	p.mu.Unlock()
	if unusable {
		return ErrPeerClosed
	}

	if err := p.stream.Write(msg); err != nil {
		return fmt.Errorf("hive: write: %w", err)
	}
	return nil
}

func (p *Peer) allocID() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	return p.nextID
}

func (p *Peer) fail(err error) {
	p.mu.Lock()
	if p.failed {
		p.mu.Unlock()
		return
	}
	p.failed = true
	if p.closed {
		p.err = ErrPeerClosed
	} else {
		p.err = err
	}
	for id, ch := range p.pending {
		close(ch)
		delete(p.pending, id)
	}
	p.mu.Unlock()

	close(p.done)
}
