// Package transport routes normalized inbound platform input into Hive
// operations.
//
// A transport owns presentation: syntax, prefixes, mentions, slash commands,
// aliases, buttons, menus, and platform-specific interaction payloads. Hive owns
// semantics: authorization, command identity, session and run mutation, results,
// and errors.
//
// The envelope types themselves live in the protocol package, because a transport
// plugin builds them and a plugin does not link the core.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/thupham/hive/internal/apierr"
	"github.com/thupham/hive/internal/control"
	"github.com/thupham/hive/internal/ids"
	"github.com/thupham/hive/internal/node"
	"github.com/thupham/hive/internal/permission"
	"github.com/thupham/hive/internal/storage"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// Router turns transport envelopes into Control API operations.
//
// The router owns the transport-neutral part of the mapping: deriving a stable
// command id from the inbound delivery, resolving the conversation's session, and
// invoking the Control API. Everything platform-specific stays in the transport.
type Router struct {
	transport string
	control   *control.Service
	store     *storage.Store
	nodes     *node.Server
	log       *slog.Logger
}

// RouterOptions configures a Router.
type RouterOptions struct {
	// Transport is the transport name, for example "slack". It is also the
	// prefix of the principals the transport may assert.
	Transport string

	Control *control.Service
	Store   *storage.Store
	Nodes   *node.Server
	Log     *slog.Logger
}

// NewRouter returns a router.
func NewRouter(opts RouterOptions) (*Router, error) {
	switch {
	case opts.Transport == "":
		return nil, errors.New("transport: a transport name is required")
	case opts.Control == nil:
		return nil, errors.New("transport: a control service is required")
	case opts.Store == nil:
		return nil, errors.New("transport: a store is required")
	case opts.Nodes == nil:
		return nil, errors.New("transport: a node server is required")
	}

	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Router{
		transport: opts.Transport,
		control:   opts.Control,
		store:     opts.Store,
		nodes:     opts.Nodes,
		log:       log,
	}, nil
}

// Outcome is what an envelope produced, for the transport to render.
type Outcome struct {
	Method    string
	CommandID string
	SessionID string
	RunID     string
	Created   bool
	Result    any
	Err       *v1.Error
}

// Handle processes one inbound envelope.
//
// A recognized command is never silently forwarded to the agent as a prompt, and
// ordinary text is never treated as a command. Which is which is decided by the
// transport's own grammar before the envelope is built.
func (r *Router) Handle(ctx context.Context, env v1.Envelope) *Outcome {
	principal := control.Principal(env.Principal)
	if principal == "" {
		return failed("", v1.Unauthorized("the transport supplied no principal"))
	}

	switch env.Kind {
	case v1.EnvelopeCommand:
		return r.handleCommand(ctx, env, principal)
	case v1.EnvelopeInteraction:
		return r.handleInteraction(ctx, env, principal)
	case v1.EnvelopeMessage:
		return r.handleMessage(ctx, env, principal)
	default:
		return failed("", v1.InvalidParams("unknown inbound kind %q", env.Kind))
	}
}

func (r *Router) handleCommand(ctx context.Context, env v1.Envelope, principal control.Principal) *Outcome {
	if env.Command == nil || env.Command.Name == "" {
		return failed("", v1.InvalidParams("the command has no name"))
	}

	method := env.Command.Method
	if method == "" {
		// A transport-level command error: the transport recognized the syntax
		// but exposes no such operation.
		return failed("", v1.NewErrorf(v1.CodeMethodNotFound, "%s/%s", r.transport, env.Command.Name))
	}

	commandID := r.commandID(env, method)

	switch method {
	case v1.MethodSessionCreate:
		params := v1.SessionCreateParams{CommandID: commandID, SourceID: env.SourceID}
		if len(env.Command.Args) > 0 {
			params.AgentID = env.Command.Args[0]
		}

		result, err := r.control.CreateSession(ctx, principal, params)
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		if err := r.bind(ctx, env, result.SessionID); err != nil {
			r.log.Warn("conversation could not be bound", "conversation", env.ConversationID, "error", err)
		}
		return &Outcome{
			Method:    method,
			CommandID: commandID,
			SessionID: result.SessionID,
			RunID:     result.RunID,
			Created:   true,
			Result:    result,
		}

	case v1.MethodSessionPrompt:
		sessionID, err := r.sessionFor(ctx, env)
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		return r.prompt(ctx, principal, commandID, env.SourceID, sessionID, joinArgs(env.Command.Args))

	case v1.MethodSessionCancel:
		sessionID, err := r.sessionFor(ctx, env)
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		err = r.control.Cancel(ctx, principal, v1.SessionCancelParams{
			CommandID: commandID,
			SessionID: sessionID,
			SourceID:  env.SourceID,
		})
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		return &Outcome{Method: method, CommandID: commandID, SessionID: sessionID, Result: map[string]any{"cancelled": true}}

	case v1.MethodSessionHandoff:
		sessionID, err := r.sessionFor(ctx, env)
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		if len(env.Command.Args) == 0 {
			return failed(commandID, v1.InvalidParams("handoff requires a target agent"))
		}

		result, err := r.control.Handoff(ctx, principal, v1.SessionHandoffParams{
			CommandID: commandID,
			SessionID: sessionID,
			AgentID:   env.Command.Args[0],
			SourceID:  env.SourceID,
		})
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		return &Outcome{
			Method:    method,
			CommandID: commandID,
			SessionID: sessionID,
			RunID:     result.TargetRunID,
			Created:   true,
			Result:    result,
		}

	case v1.MethodSessionConfig:
		sessionID, err := r.sessionFor(ctx, env)
		if err != nil {
			return failed(commandID, apierr.From(err))
		}

		result, err := r.control.SessionConfig(ctx, principal, v1.SessionConfigParams{
			CommandID: commandID,
			SessionID: sessionID,
			ConfigID:  env.Command.ConfigID,
			Value:     strings.Join(env.Command.Args, " "),
			SourceID:  env.SourceID,
		})
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		return &Outcome{
			Method:    method,
			CommandID: commandID,
			SessionID: sessionID,
			Result:    result,
		}

	case v1.MethodSessionStatus:
		sessionID, err := r.sessionFor(ctx, env)
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		result, err := r.control.Status(ctx, principal, sessionID)
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		return &Outcome{Method: method, CommandID: commandID, SessionID: sessionID, Result: result}

	case v1.MethodSessionEvents:
		sessionID, err := r.sessionFor(ctx, env)
		if err != nil {
			return failed(commandID, apierr.From(err))
		}

		result, err := r.control.Replay(ctx, principal, v1.EventReplayParams{
			SessionID:    sessionID,
			FromSequence: logFrom(env.Command.Args),
			Limit:        logLimit(env.Command.Args),
		})
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		return &Outcome{
			Method:    method,
			CommandID: commandID,
			SessionID: sessionID,
			Result:    result,
		}

	case v1.MethodSessionList:
		result, err := r.control.ListSessions(ctx, principal, 0)
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		return &Outcome{Method: method, CommandID: commandID, Result: result}

	case v1.MethodAgentList:
		result, err := r.control.ListAgents(ctx, principal)
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		return &Outcome{Method: method, CommandID: commandID, Result: result}

	case v1.MethodNodeList:
		result, err := r.control.ListNodes(ctx, principal)
		if err != nil {
			return failed(commandID, apierr.From(err))
		}
		return &Outcome{Method: method, CommandID: commandID, Result: result}

	default:
		// The operation is named in the transport map but is not implemented
		// here. Report it rather than silently doing nothing.
		return failed(commandID, v1.Unsupported("%s maps %q to %s, which is not implemented",
			r.transport, env.Command.Name, method))
	}
}

func (r *Router) handleInteraction(ctx context.Context, env v1.Envelope, principal control.Principal) *Outcome {
	if env.Interaction == nil {
		return failed("", v1.InvalidParams("the interaction is empty"))
	}

	commandID := r.commandID(env, env.Interaction.Action)
	sessionID, err := r.sessionFor(ctx, env)
	if err != nil {
		return failed(commandID, apierr.From(err))
	}

	switch env.Interaction.Method {
	case v1.MethodPermissionRespond:
		var params v1.PermissionRespondParams
		if err := json.Unmarshal([]byte(env.Interaction.Value), &params); err != nil {
			return failed(commandID, v1.InvalidParams("invalid permission response"))
		}
		if err := r.respondToPermission(ctx, principal, sessionID, params); err != nil {
			return failed(commandID, apierr.From(err))
		}
		return &Outcome{
			Method:    env.Interaction.Action,
			CommandID: commandID,
			SessionID: sessionID,
			Result:    map[string]any{"resolved": true},
		}

	default:
		// A transport that recognized the press but mapped it to nothing is a
		// transport bug, and it says so rather than doing nothing.
		return failed(commandID, v1.NewErrorf(v1.CodeMethodNotFound,
			"%s: action %q has no operation", r.transport, env.Interaction.Action))
	}
}

// handleMessage turns ordinary text into a prompt.
//
// A conversation with no session yet gets one, because the first thing a user
// says has to land somewhere. That is a routing decision, not an accident of
// receiving text.
func (r *Router) handleMessage(ctx context.Context, env v1.Envelope, principal control.Principal) *Outcome {
	if env.Message == nil || env.Message.Text == "" {
		return failed("", v1.InvalidParams("the message is empty"))
	}

	commandID := r.commandID(env, v1.MethodSessionPrompt)

	sessionID, err := r.sessionFor(ctx, env)
	if errors.Is(err, storage.ErrNotFound) {
		created, createErr := r.control.CreateSession(ctx, principal, v1.SessionCreateParams{
			CommandID: commandID + ":session",
			SourceID:  env.SourceID,
		})
		if createErr != nil {
			return failed(commandID, apierr.From(createErr))
		}
		if err := r.bind(ctx, env, created.SessionID); err != nil {
			r.log.Warn("conversation could not be bound", "conversation", env.ConversationID, "error", err)
		}
		sessionID = created.SessionID
	} else if err != nil {
		return failed(commandID, apierr.From(err))
	}

	return r.promptWithContext(ctx, principal, commandID, env.SourceID, sessionID,
		env.Message.Text, env.ChannelContext, env.Message.Attachments)
}

// logFrom reads an optional starting sequence from a command argument.
func logFrom(args []string) int64 {
	if len(args) == 0 {
		return 0
	}
	value, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

// logLimit is how much activity to show.
func logLimit(args []string) int {
	if len(args) > 1 {
		if value, err := strconv.Atoi(strings.TrimSpace(args[1])); err == nil && value > 0 {
			return value
		}
	}
	return defaultLogLimit
}

// defaultLogLimit bounds how much a trace shows.
const defaultLogLimit = 60

// renderRoom describes the surrounding conversation.
//
// It is bounded and labelled: an agent must be able to tell what it was told from
// what it was asked, and must not read its own output back as a person speaking.
func renderRoom(room []v1.ChannelMessage) string {
	if len(room) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Recent messages in this conversation, oldest first. This is context, not the request.\n")
	for _, message := range room {
		author := message.Author
		if message.Self {
			author = "this agent"
		}
		if author == "" {
			author = "someone"
		}

		text := strings.Join(strings.Fields(message.Text), " ")
		if text == "" {
			continue
		}
		if len(text) > 400 {
			text = text[:400] + "…"
		}
		fmt.Fprintf(&b, "- %s: %s\n", author, text)
	}

	b.WriteString("\nThe request follows.")
	return b.String()
}

func (r *Router) prompt(ctx context.Context, principal control.Principal, commandID, sourceID, sessionID, text string) *Outcome {
	return r.promptWithContext(ctx, principal, commandID, sourceID, sessionID, text, nil, nil)
}

// promptImages selects the pictures to send with a prompt.
//
// A file that is not an image is named in the text instead, so the agent knows
// something was attached even though it cannot be handed the bytes.
func promptImages(attachments []v1.Attachment) []v1.PromptImage {
	var out []v1.PromptImage
	for _, attachment := range attachments {
		if !attachment.IsImage() || len(attachment.Data) == 0 {
			continue
		}
		out = append(out, v1.PromptImage{
			MimeType: attachment.MimeType,
			Data:     attachment.Data,
			Name:     attachment.Name,
		})
	}
	return out
}

// promptWithContext sends a prompt together with what else is being said.
//
// The core renders the preamble, so a transport does not decide how Hive
// describes a room to an agent.
func (r *Router) promptWithContext(ctx context.Context, principal control.Principal, commandID, sourceID, sessionID, text string, room []v1.ChannelMessage, attachments []v1.Attachment) *Outcome {
	result, err := r.control.Prompt(ctx, principal, v1.SessionPromptParams{
		CommandID: commandID,
		SessionID: sessionID,
		Text:      text,
		SourceID:  sourceID,
		Context:   renderRoom(room),
		Images:    promptImages(attachments),
	})
	if err != nil {
		return failed(commandID, apierr.From(err))
	}
	return &Outcome{
		Method:    v1.MethodSessionPrompt,
		CommandID: commandID,
		SessionID: sessionID,
		RunID:     result.RunID,
		Created:   result.CreatedRun,
		Result:    result,
	}
}

// respondToPermission delegates to the Control API.
//
// Resolution is a domain operation: the transport supplies the authenticated
// principal and the agent's request id, and Hive does the rest.
func (r *Router) respondToPermission(ctx context.Context, principal control.Principal, sessionID string, params v1.PermissionRespondParams) error {
	params.SessionID = sessionID
	return r.control.RespondToPermission(ctx, principal, params)
}

// sessionFor resolves the conversation's session, or ErrNotFound.
func (r *Router) sessionFor(ctx context.Context, env v1.Envelope) (string, error) {
	binding, err := r.store.GetBinding(ctx, env.Transport, env.ConversationID)
	if err != nil {
		return "", err
	}
	return binding.SessionID, nil
}

// bind records that a conversation belongs to a session.
func (r *Router) bind(ctx context.Context, env v1.Envelope, sessionID string) error {
	binding := &storage.Binding{
		ID:             ids.New("bind"),
		SessionID:      sessionID,
		Transport:      env.Transport,
		ConversationID: env.ConversationID,
		Principal:      env.Principal,
	}
	return r.store.WriteTx(ctx, func(tx storage.Execer) error {
		return r.store.UpsertBinding(ctx, tx, binding)
	})
}

// commandID derives the idempotency key for an inbound delivery.
//
// A platform redelivery reuses SourceID, so it derives the same command id and
// resolves to the existing operation. A user deliberately repeating an action
// produces a new SourceID and therefore a new command. Without a SourceID the
// delivery cannot be deduplicated, so a fresh id is generated.
func (r *Router) commandID(env v1.Envelope, method string) string {
	if env.SourceID == "" {
		return ids.New("cmd")
	}
	return fmt.Sprintf("%s:%s:%s", env.Transport, env.SourceID, method)
}

func joinArgs(args []string) string {
	out := ""
	for i, arg := range args {
		if i > 0 {
			out += " "
		}
		out += arg
	}
	return out
}

// failed builds an outcome carrying a protocol error.
func failed(commandID string, err error) *Outcome {
	return &Outcome{CommandID: commandID, Err: v1.AsError(err)}
}

func permissionState(approved bool) permission.State {
	if approved {
		return permission.StateApproved
	}
	return permission.StateDenied
}
