// Package acp implements the Agent Client Protocol as a Hive agent runtime
// adapter.
//
// This package is the ACP compatibility boundary. Hive core does not
// version-convert ACP: when the protocol changes, this adapter changes, and the
// Hive session model does not.
//
// It must not import any internal Hive package. An agent adapter is a plugin,
// and a plugin does not link the core.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// Transport is the byte stream to an agent process. ACP runs over stdio.
type Transport interface {
	io.Reader
	io.Writer
	io.Closer
}

// ErrClosed reports that the connection is no longer usable.
var ErrClosed = errors.New("acp: connection closed")

// Options configures a Client.
type Options struct {
	// ClientVersion is reported to the agent.
	ClientVersion string

	// UpdateBuffer is the session update queue depth. A full queue drops
	// updates rather than stalling the connection, because the durable event
	// path is separate from the realtime one.
	UpdateBuffer int

	// PermissionBuffer is the permission request queue depth.
	PermissionBuffer int
}

// Client is an ACP connection to one agent.
//
// A Client is safe for concurrent use. It is not safe to reuse after Close.
type Client struct {
	transport Transport
	opts      Options
	enc       *json.Encoder

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcMessage
	closed  bool
	failed  bool
	err     error

	updates        chan Update
	permissions    chan PermissionRequest
	droppedUpdates atomic.Int64

	done chan struct{}
	wg   sync.WaitGroup
}

// NewClient starts a client on the given transport.
func NewClient(transport Transport, opts Options) *Client {
	if opts.ClientVersion == "" {
		opts.ClientVersion = "0.0.0-dev"
	}
	if opts.UpdateBuffer <= 0 {
		opts.UpdateBuffer = 256
	}
	if opts.PermissionBuffer <= 0 {
		opts.PermissionBuffer = 32
	}

	c := &Client{
		transport:   transport,
		opts:        opts,
		enc:         json.NewEncoder(transport),
		pending:     make(map[int64]chan rpcMessage),
		updates:     make(chan Update, opts.UpdateBuffer),
		permissions: make(chan PermissionRequest, opts.PermissionBuffer),
		done:        make(chan struct{}),
	}
	c.wg.Add(1)
	go c.readLoop()
	return c
}

// Updates streams session notifications. The channel is closed when the
// connection ends.
func (c *Client) Updates() <-chan Update { return c.updates }

// Permissions streams permission requests awaiting a response. The channel is
// closed when the connection ends.
func (c *Client) Permissions() <-chan PermissionRequest { return c.permissions }

// DroppedUpdates returns how many updates were dropped because the queue was
// full.
func (c *Client) DroppedUpdates() int64 { return c.droppedUpdates.Load() }

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the error that ended the connection, if any.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Initialize negotiates the protocol version and capabilities.
//
// An unsupported protocol version fails the handshake explicitly and closes
// the connection: the ACP specification requires the client to disconnect
// rather than guess at another version's semantics.
func (c *Client) Initialize(ctx context.Context) (*Capabilities, error) {
	req := initializeRequest{
		ProtocolVersion: ProtocolVersion,
		ClientCapabilities: clientCapabilities{
			FS:       fsCapabilities{},
			Terminal: false,
		},
		ClientInfo: &Implementation{Name: "hive", Title: "Hive", Version: c.opts.ClientVersion},
	}

	var resp initializeResponse
	if err := c.call(ctx, methodInitialize, req, &resp); err != nil {
		return nil, err
	}

	if resp.ProtocolVersion != ProtocolVersion {
		_ = c.Close()
		return nil, fmt.Errorf("acp: agent negotiated protocol version %d, this adapter supports %d",
			resp.ProtocolVersion, ProtocolVersion)
	}

	caps := &Capabilities{
		ProtocolVersion: resp.ProtocolVersion,
		AuthMethods:     resp.AuthMethods,
		LoadSession:     resp.AgentCapabilities.LoadSession,
		PromptImages:    resp.AgentCapabilities.PromptCapabilities.Image,
		PromptAudio:     resp.AgentCapabilities.PromptCapabilities.Audio,
		PromptEmbedded:  resp.AgentCapabilities.PromptCapabilities.EmbeddedContext,
	}
	if resp.AgentInfo != nil {
		caps.AgentName = resp.AgentInfo.Name
		caps.AgentVersion = resp.AgentInfo.Version
	}
	return caps, nil
}

// Authenticate selects one of the agent's advertised auth methods.
//
// An agent that advertises auth methods refuses to create a session until this
// succeeds. Which method to use is the caller's decision, because it depends on
// credentials Hive does not own.
func (c *Client) Authenticate(ctx context.Context, methodID string, meta json.RawMessage) error {
	if methodID == "" {
		return errors.New("acp: an auth method id is required")
	}
	return c.call(ctx, methodAuthenticate, authenticateRequest{MethodID: methodID, Meta: meta}, nil)
}

// NewSession creates an agent session and returns its runtime session id.
func (c *Client) NewSession(ctx context.Context, req NewSessionRequest) (*NewSessionResult, error) {
	if req.Cwd == "" {
		return nil, errors.New("acp: session cwd is required")
	}
	if req.McpServers == nil {
		req.McpServers = []json.RawMessage{}
	}

	var resp newSessionResponse
	if err := c.call(ctx, methodSessionNew, req, &resp); err != nil {
		return nil, err
	}
	if resp.SessionID == "" {
		return nil, errors.New("acp: agent returned an empty session id")
	}
	return &NewSessionResult{SessionID: resp.SessionID, ConfigOptions: resp.ConfigOptions}, nil
}

// SetConfigOption changes one session-level setting.
//
// The agent declares what it offers and what the values are, so this is how a
// client selects a model without knowing anything about models.
func (c *Client) SetConfigOption(ctx context.Context, sessionID, configID string, value any) error {
	if configID == "" {
		return errors.New("acp: a config id is required")
	}
	return c.call(ctx, methodSetConfigOption, setConfigOptionRequest{
		SessionID: sessionID,
		ConfigID:  configID,
		Value:     value,
	}, nil)
}

// LoadSession restores a session the agent created earlier.
//
// This is the same-agent restore path. It must only be used with a runtime
// session id that belongs to this same runtime.
func (c *Client) LoadSession(ctx context.Context, req LoadSessionRequest) error {
	switch {
	case req.SessionID == "":
		return errors.New("acp: session id is required")
	case req.Cwd == "":
		return errors.New("acp: session cwd is required")
	}
	if req.McpServers == nil {
		req.McpServers = []json.RawMessage{}
	}
	return c.call(ctx, methodSessionLoad, req, nil)
}

// Prompt sends a text prompt and waits for the turn to finish, returning the
// agent's stop reason.
func (c *Client) Prompt(ctx context.Context, sessionID string, content []Content) (string, error) {
	if sessionID == "" {
		return "", errors.New("acp: session id is required")
	}

	blocks := make([]contentBlock, 0, len(content))
	for _, block := range content {
		switch {
		case len(block.Data) > 0:
			blocks = append(blocks, contentBlock{
				Type:     "image",
				Data:     block.Data,
				MimeType: block.MimeType,
			})
		case block.Text != "":
			blocks = append(blocks, contentBlock{Type: "text", Text: block.Text})
		}
	}
	if len(blocks) == 0 {
		return "", errors.New("acp: a prompt needs at least one content block")
	}

	req := promptRequest{SessionID: sessionID, Prompt: blocks}

	var resp promptResponse
	if err := c.call(ctx, methodSessionPrompt, req, &resp); err != nil {
		return "", err
	}
	return resp.StopReason, nil
}

// Cancel notifies the agent to cancel the current turn.
func (c *Client) Cancel(sessionID string) error {
	if sessionID == "" {
		return errors.New("acp: session id is required")
	}
	return c.write(rpcMessage{
		JSONRPC: jsonrpcVersion,
		Method:  methodSessionCancel,
		Params:  encode(cancelNotification{SessionID: sessionID}),
	})
}

// RespondToPermission answers a permission request.
//
// requestID is the raw id from the PermissionRequest, echoed unchanged.
func (c *Client) RespondToPermission(requestID json.RawMessage, outcome PermissionOutcome) error {
	if len(requestID) == 0 {
		return errors.New("acp: permission request id is required")
	}
	return c.write(rpcMessage{
		JSONRPC: jsonrpcVersion,
		ID:      requestID,
		Result:  encode(outcome),
	})
}

// Close shuts the connection down and fails any call still in flight.
func (c *Client) Close() error {
	c.mu.Lock()
	alreadyClosed := c.closed
	c.closed = true
	c.mu.Unlock()

	if alreadyClosed {
		return nil
	}

	err := c.transport.Close()
	c.fail(ErrClosed)
	c.wg.Wait()
	return err
}

func (c *Client) readLoop() {
	defer c.wg.Done()
	defer close(c.updates)
	defer close(c.permissions)

	dec := json.NewDecoder(c.transport)
	for {
		var msg rpcMessage
		if err := dec.Decode(&msg); err != nil {
			c.fail(fmt.Errorf("acp: read: %w", err))
			return
		}
		c.dispatch(msg)
	}
}

func (c *Client) dispatch(msg rpcMessage) {
	switch {
	case msg.Method != "" && len(msg.ID) > 0:
		c.handleRequest(msg)
	case msg.Method != "":
		c.handleNotification(msg)
	default:
		c.handleResponse(msg)
	}
}

func (c *Client) handleResponse(msg rpcMessage) {
	id, ok := decodeID(msg.ID)
	if !ok {
		return
	}

	c.mu.Lock()
	ch, found := c.pending[id]
	if found {
		delete(c.pending, id)
	}
	c.mu.Unlock()

	if found {
		// Buffered, so a response never blocks the read loop.
		ch <- msg
	}
}

// handleNotification preserves protocol data Hive does not interpret.
func (c *Client) handleNotification(msg rpcMessage) {
	var params struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	_ = json.Unmarshal(msg.Params, &params)

	payload := msg.Params
	if len(params.Update) > 0 {
		payload = params.Update
	}

	select {
	case c.updates <- Update{SessionID: params.SessionID, Method: msg.Method, Payload: payload}:
	default:
		// The realtime path may lose updates; the durable path is separate
		// and authoritative.
		c.droppedUpdates.Add(1)
	}
}

func (c *Client) handleRequest(msg rpcMessage) {
	if msg.Method != methodRequestPermission {
		// Hive advertises no filesystem, terminal, or elicitation
		// capabilities, so a conforming agent will not call these. Answering
		// with an error keeps the connection well-formed and fails closed.
		c.respondError(msg.ID, codeMethodNotFound, "method not supported by hive: "+msg.Method)
		return
	}

	var params struct {
		SessionID string             `json:"sessionId"`
		ToolCall  json.RawMessage    `json:"toolCall"`
		Options   []PermissionOption `json:"options"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		c.respondError(msg.ID, codeInvalidParams, "invalid permission request")
		return
	}

	select {
	case c.permissions <- PermissionRequest{
		RequestID: msg.ID,
		SessionID: params.SessionID,
		ToolCall:  params.ToolCall,
		Options:   params.Options,
	}:
	default:
		// Hive could not take delivery of the request. Answering with
		// cancelled fails closed: the agent aborts the operation instead of
		// proceeding with an approval nobody gave.
		_ = c.write(rpcMessage{
			JSONRPC: jsonrpcVersion,
			ID:      msg.ID,
			Result:  encode(PermissionOutcome{Outcome: outcomeCancelled}),
		})
	}
}

func (c *Client) respondError(id json.RawMessage, code int, message string) {
	_ = c.write(rpcMessage{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	})
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	id := c.allocID()
	ch := make(chan rpcMessage, 1)

	c.mu.Lock()
	if c.closed || c.failed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	msg := rpcMessage{JSONRPC: jsonrpcVersion, ID: encode(id), Method: method}
	if params != nil {
		msg.Params = encode(params)
	}
	if err := c.write(msg); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case reply, ok := <-ch:
		if !ok {
			return c.Err()
		}
		if reply.Error != nil {
			return reply.Error
		}
		if result != nil && len(reply.Result) > 0 {
			if err := json.Unmarshal(reply.Result, result); err != nil {
				return fmt.Errorf("acp: decode %s result: %w", method, err)
			}
		}
		return nil
	}
}

func (c *Client) write(msg rpcMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	unusable := c.closed || c.failed
	c.mu.Unlock()
	if unusable {
		return ErrClosed
	}

	if err := c.enc.Encode(msg); err != nil {
		return fmt.Errorf("acp: write: %w", err)
	}
	return nil
}

func (c *Client) allocID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

// fail ends the connection at most once and releases every waiting call.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.failed {
		c.mu.Unlock()
		return
	}
	c.failed = true
	if c.closed {
		c.err = ErrClosed
	} else {
		c.err = err
	}
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()

	close(c.done)
}

// decodeID normalizes a JSON-RPC id, which may be a number or a string.
func decodeID(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
