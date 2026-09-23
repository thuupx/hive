package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// DefaultHandshakeTimeout bounds how long a plugin has to say hello.
const DefaultHandshakeTimeout = 5 * time.Second

// StopGracePeriod is how long a plugin gets to exit after being asked.
const StopGracePeriod = 2 * time.Second

// Spec describes a plugin to supervise.
type Spec struct {
	// ID is the stable plugin identity. It survives restarts; a process
	// instance does not.
	ID string

	Type    v1.PluginType
	Version string
	Command []string
	Env     []string

	// MaxRestarts bounds how many times a crashing plugin is restarted. Zero
	// means it is not restarted.
	MaxRestarts int

	// RestartDelay is the pause before a restart. Zero uses a default.
	RestartDelay time.Duration
}

// Handler serves a plugin-invoked method.
type Handler func(ctx context.Context, call Call) (any, error)

// Call is a method a plugin invoked on the core.
type Call struct {
	Instance *Instance
	Method   string
	Params   json.RawMessage
}

// Instance is one live plugin process.
//
// A stable plugin identity and a plugin process instance are distinct: the
// identity keeps its trust and its capabilities, while each process instance
// gets a fresh connection generation.
type Instance struct {
	ID                   string
	PluginID             string
	Type                 v1.PluginType
	Version              string
	ConnectionGeneration int64
	Capabilities         []string
	StartedAt            time.Time

	peer *v1.Peer
}

// Call invokes a method on the plugin.
func (i *Instance) Call(ctx context.Context, method string, params, result any) error {
	return i.peer.Call(ctx, method, params, result)
}

// Notify sends a notification to the plugin.
func (i *Instance) Notify(method string, params any) error {
	return i.peer.Notify(method, params)
}

// Done is closed when the plugin connection ends.
func (i *Instance) Done() <-chan struct{} { return i.peer.Done() }

// Err returns the error that ended the plugin connection, if any.
func (i *Instance) Err() error { return i.peer.Err() }

// Supervisor owns the plugin processes.
type Supervisor struct {
	log *slog.Logger

	// HandshakeTimeout bounds how long a plugin has to say hello. Zero uses
	// DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration

	mu       sync.Mutex
	managed  map[string]*managed
	handlers map[string]Handler
}

type managed struct {
	spec Spec

	mu          sync.Mutex
	instance    *Instance
	generations int64
	restarts    int
	stopped     bool
	cmd         *exec.Cmd
	procDone    chan struct{}
}

// NewSupervisor returns a supervisor. A nil logger discards output.
func NewSupervisor(log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Supervisor{
		log:      log,
		managed:  make(map[string]*managed),
		handlers: make(map[string]Handler),
	}
}

// Handle registers the handler for a core method a plugin may invoke.
func (s *Supervisor) Handle(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// Start launches a plugin and completes its handshake.
func (s *Supervisor) Start(ctx context.Context, spec Spec) (*Instance, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}

	m := &managed{spec: spec}

	s.mu.Lock()
	if _, exists := s.managed[spec.ID]; exists {
		s.mu.Unlock()
		return nil, fmt.Errorf("plugin: %s is already supervised", spec.ID)
	}
	s.managed[spec.ID] = m
	s.mu.Unlock()

	inst, err := s.launch(ctx, m)
	if err != nil {
		s.mu.Lock()
		delete(s.managed, spec.ID)
		s.mu.Unlock()
		return nil, err
	}
	return inst, nil
}

// Stop asks a plugin to exit and stops supervising it.
func (s *Supervisor) Stop(pluginID string) error {
	s.mu.Lock()
	m, ok := s.managed[pluginID]
	if ok {
		delete(s.managed, pluginID)
	}
	s.mu.Unlock()

	if !ok {
		return fmt.Errorf("plugin: %s is not supervised", pluginID)
	}

	m.mu.Lock()
	m.stopped = true
	inst := m.instance
	m.instance = nil
	cmd := m.cmd
	done := m.procDone
	m.mu.Unlock()

	if inst == nil {
		return nil
	}

	// Ask politely, then close the pipe, then kill if the process is still
	// alive. A plugin must not be able to hold shutdown open indefinitely.
	_ = inst.Notify(v1.MethodPluginShutdown, nil)
	_ = inst.peer.Close()

	if cmd == nil || cmd.Process == nil || done == nil {
		return nil
	}
	select {
	case <-done:
	case <-time.After(StopGracePeriod):
		_ = cmd.Process.Kill()
		<-done
	}
	return nil
}

// StopAll stops every supervised plugin.
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.managed))
	for id := range s.managed {
		ids = append(ids, id)
	}
	s.mu.Unlock()

	for _, id := range ids {
		_ = s.Stop(id)
	}
}

// Instance returns the current instance for a plugin identity.
func (s *Supervisor) Instance(pluginID string) (*Instance, bool) {
	s.mu.Lock()
	m, ok := s.managed[pluginID]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.instance == nil {
		return nil, false
	}
	return m.instance, true
}

// Restarts returns how many times a plugin has been restarted.
func (s *Supervisor) Restarts(pluginID string) int {
	s.mu.Lock()
	m, ok := s.managed[pluginID]
	s.mu.Unlock()
	if !ok {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restarts
}

// Authorize reports whether an instance may invoke method.
//
// It rejects an instance that is no longer current for its plugin identity, so
// a superseded connection cannot keep invoking capabilities after a restart.
func (s *Supervisor) Authorize(inst *Instance, method string) error {
	if inst == nil {
		return v1.Unauthorized("nil plugin instance")
	}
	if !s.isCurrent(inst) {
		return v1.Unauthorized("plugin instance %s is superseded", inst.ID)
	}
	return AuthorizeMethod(inst.Capabilities, method)
}

func (s *Supervisor) isCurrent(inst *Instance) bool {
	s.mu.Lock()
	m, ok := s.managed[inst.PluginID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instance == inst
}

func (s *Supervisor) launch(ctx context.Context, m *managed) (*Instance, error) {
	cmd := exec.Command(m.spec.Command[0], m.spec.Command[1:]...)
	cmd.Env = append(os.Environ(), m.spec.Env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: %s: stdin: %w", m.spec.ID, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: %s: stdout: %w", m.spec.ID, err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin: %s: start: %w", m.spec.ID, err)
	}

	procDone := make(chan struct{})
	m.mu.Lock()
	m.cmd = cmd
	m.procDone = procDone
	m.generations++
	generation := m.generations
	m.mu.Unlock()

	stream := v1.NewStream(stdout, stdin, stdout, stdin)

	hello, helloID, err := s.handshake(ctx, m.spec, stream)
	if err != nil {
		_ = stream.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		close(procDone)
		return nil, err
	}

	inst := &Instance{
		ID:                   fmt.Sprintf("%s#%d", m.spec.ID, generation),
		PluginID:             m.spec.ID,
		Type:                 m.spec.Type,
		Version:              hello.Version,
		ConnectionGeneration: generation,
		Capabilities:         Grant(m.spec.Type, hello.Capabilities),
		StartedAt:            time.Now().UTC(),
	}

	ready := v1.HelloResponse{
		InstanceID:           inst.ID,
		ConnectionGeneration: inst.ConnectionGeneration,
		Capabilities:         inst.Capabilities,
	}
	resp, err := v1.NewResult(helloID, ready)
	if err != nil {
		_ = stream.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		close(procDone)
		return nil, fmt.Errorf("plugin: %s: ready: %w", m.spec.ID, err)
	}
	if err := stream.Write(resp); err != nil {
		_ = stream.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		close(procDone)
		return nil, fmt.Errorf("plugin: %s: ready: %w", m.spec.ID, err)
	}

	peer := v1.NewPeer(stream)
	inst.peer = peer
	peer.Start()

	m.mu.Lock()
	m.instance = inst
	m.mu.Unlock()

	s.serve(inst)

	go s.watch(m, cmd, procDone)

	s.log.Info("plugin ready",
		"plugin", m.spec.ID,
		"type", string(m.spec.Type),
		"instance", inst.ID,
		"generation", inst.ConnectionGeneration,
		"capabilities", len(inst.Capabilities),
	)
	return inst, nil
}

func (s *Supervisor) handshake(ctx context.Context, spec Spec, stream *v1.Stream) (*v1.HelloRequest, v1.ID, error) {
	timeout := s.HandshakeTimeout
	if timeout <= 0 {
		timeout = DefaultHandshakeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
		return nil, v1.ID{}, fmt.Errorf("plugin: %s: handshake timed out", spec.ID)
	case r := <-ch:
		if r.err != nil {
			return nil, v1.ID{}, fmt.Errorf("plugin: %s: handshake read: %w", spec.ID, r.err)
		}
		if r.msg.Kind() != v1.KindRequest || r.msg.Method != v1.MethodPluginHello {
			return nil, v1.ID{}, fmt.Errorf("plugin: %s: expected a %s request, got %q",
				spec.ID, v1.MethodPluginHello, r.msg.Method)
		}

		var hello v1.HelloRequest
		if err := json.Unmarshal(r.msg.Params, &hello); err != nil {
			return nil, v1.ID{}, fmt.Errorf("plugin: %s: invalid hello: %w", spec.ID, err)
		}
		if err := validateHello(spec, hello); err != nil {
			return nil, v1.ID{}, err
		}
		return &hello, r.msg.RequestID(), nil
	}
}

func (s *Supervisor) serve(inst *Instance) {
	go func() {
		for {
			select {
			case <-inst.peer.Done():
				return
			case req, ok := <-inst.peer.Requests():
				if !ok {
					return
				}
				s.handleRequest(inst, req)
			case note, ok := <-inst.peer.Notifications():
				if !ok {
					return
				}
				s.log.Debug("plugin notification", "plugin", inst.PluginID, "method", note.Method)
			}
		}
	}()
}

func (s *Supervisor) handleRequest(inst *Instance, req *v1.Message) {
	id := req.RequestID()

	if err := s.Authorize(inst, req.Method); err != nil {
		_ = inst.peer.RespondError(id, v1.AsError(err))
		return
	}

	s.mu.Lock()
	h := s.handlers[req.Method]
	s.mu.Unlock()

	if h == nil {
		_ = inst.peer.RespondError(id, v1.MethodNotFound(req.Method))
		return
	}

	result, err := h(context.Background(), Call{Instance: inst, Method: req.Method, Params: req.Params})
	if err != nil {
		_ = inst.peer.RespondError(id, v1.AsError(err))
		return
	}
	if err := inst.peer.Respond(id, result); err != nil {
		s.log.Warn("plugin response failed", "plugin", inst.PluginID, "method", req.Method, "error", err)
	}
}

// watch detects a plugin exit and restarts it according to policy.
//
// A plugin crash must never terminate the coordinator or unrelated sessions, so
// this runs entirely outside the core request path.
func (s *Supervisor) watch(m *managed, cmd *exec.Cmd, procDone chan struct{}) {
	err := cmd.Wait()
	close(procDone)

	m.mu.Lock()
	stopped := m.stopped
	restarts := m.restarts
	m.mu.Unlock()

	if stopped {
		return
	}

	s.log.Warn("plugin exited", "plugin", m.spec.ID, "error", err)

	if restarts >= m.spec.MaxRestarts {
		s.log.Error("plugin will not be restarted",
			"plugin", m.spec.ID, "restarts", restarts, "max", m.spec.MaxRestarts)
		return
	}

	delay := m.spec.RestartDelay
	if delay <= 0 {
		delay = time.Second
	}

	go func() {
		time.Sleep(delay)

		m.mu.Lock()
		if m.stopped {
			m.mu.Unlock()
			return
		}
		m.restarts++
		restarts := m.restarts
		m.mu.Unlock()

		s.log.Info("restarting plugin", "plugin", m.spec.ID, "restart", restarts)
		if _, err := s.launch(context.Background(), m); err != nil {
			s.log.Error("plugin restart failed", "plugin", m.spec.ID, "error", err)
		}
	}()
}

func validateSpec(spec Spec) error {
	switch {
	case spec.ID == "":
		return errors.New("plugin: spec id is required")
	case !spec.Type.Valid():
		return fmt.Errorf("plugin: %s: unknown plugin type %q", spec.ID, spec.Type)
	case len(spec.Command) == 0:
		return fmt.Errorf("plugin: %s: command is required", spec.ID)
	case spec.MaxRestarts < 0:
		return fmt.Errorf("plugin: %s: max restarts must not be negative", spec.ID)
	}
	return nil
}

func validateHello(spec Spec, hello v1.HelloRequest) error {
	switch {
	case hello.PluginID != spec.ID:
		return fmt.Errorf("plugin: %s: hello declared identity %q", spec.ID, hello.PluginID)
	case hello.Type != spec.Type:
		return fmt.Errorf("plugin: %s: hello declared type %q, want %q", spec.ID, hello.Type, spec.Type)
	case !hello.ProtocolVersion.Compatible():
		return fmt.Errorf("plugin: %s: incompatible protocol version %s", spec.ID, hello.ProtocolVersion)
	}
	return nil
}
