package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// Gateway dispatches Control API requests on an authenticated connection.
//
// Authorization is evaluated here, before a mutating operation is accepted, and
// the acting principal is resolved from the connection rather than from anything
// the caller merely claims.
type Gateway struct {
	service *Service
	log     *slog.Logger
}

// NewGateway returns a gateway over a service.
func NewGateway(service *Service, log *slog.Logger) *Gateway {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Gateway{service: service, log: log}
}

// Service exposes the underlying service.
func (g *Gateway) Service() *Service { return g.service }

// Serve handles Control API requests until ctx is cancelled or the connection
// ends.
func (g *Gateway) Serve(ctx context.Context, peer *v1.Peer, conn Connection) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-peer.Done():
			return nil
		case req, ok := <-peer.Requests():
			if !ok {
				return nil
			}
			g.serveOne(ctx, peer, conn, req)
		}
	}
}

func (g *Gateway) serveOne(ctx context.Context, peer *v1.Peer, conn Connection, req *v1.Message) {
	id := req.RequestID()

	result, err := g.Handle(ctx, conn, req)
	if err != nil {
		_ = peer.RespondError(id, v1.AsError(err))
		return
	}
	if err := peer.Respond(id, result); err != nil {
		g.log.Warn("control response failed", "method", req.Method, "error", err)
	}
}

// Handle serves one request and returns its result.
//
// It is exported so a transport plugin can reuse the same dispatch without
// wrapping it in a peer.
func (g *Gateway) Handle(ctx context.Context, conn Connection, req *v1.Message) (any, error) {
	principal, err := conn.ActingPrincipal(actorOf(req))
	if err != nil {
		return nil, err
	}
	return g.HandleAs(ctx, principal, req)
}

// HandleAs serves one request as an already-resolved principal.
//
// A caller that established the principal itself — a trusted transport that
// validated its own assertion, for instance — uses this instead of Handle, which
// resolves the principal from the connection.
func (g *Gateway) HandleAs(ctx context.Context, principal Principal, req *v1.Message) (any, error) {
	switch req.Method {
	case v1.MethodSessionCreate:
		var params v1.SessionCreateParams
		if err := decode(req, &params); err != nil {
			return nil, err
		}
		return g.service.CreateSession(ctx, principal, params)

	case v1.MethodSessionPrompt:
		var params v1.SessionPromptParams
		if err := decode(req, &params); err != nil {
			return nil, err
		}
		return g.service.Prompt(ctx, principal, params)

	case v1.MethodSessionHandoff:
		var params v1.SessionHandoffParams
		if err := decode(req, &params); err != nil {
			return nil, err
		}
		return g.service.Handoff(ctx, principal, params)

	case v1.MethodSessionCancel:
		var params v1.SessionCancelParams
		if err := decode(req, &params); err != nil {
			return nil, err
		}
		if err := g.service.Cancel(ctx, principal, params); err != nil {
			return nil, err
		}
		return map[string]any{"cancelled": true}, nil

	case v1.MethodSessionStatus:
		var params v1.SessionStatusParams
		if err := decode(req, &params); err != nil {
			return nil, err
		}
		return g.service.Status(ctx, principal, params.SessionID)

	case v1.MethodSessionList:
		var params v1.SessionListParams
		if err := decode(req, &params); err != nil {
			return nil, err
		}
		return g.service.ListSessions(ctx, principal, params.Limit)

	case v1.MethodSessionEvents:
		var params v1.EventReplayParams
		if err := decode(req, &params); err != nil {
			return nil, err
		}
		return g.service.Replay(ctx, principal, params)

	case v1.MethodWorkspaceCreate:
		var params v1.WorkspaceCreateParams
		if err := decode(req, &params); err != nil {
			return nil, err
		}
		return g.service.CreateWorkspace(ctx, principal, params)

	case v1.MethodWorkspaceList:
		return g.service.ListWorkspaces(ctx, principal)

	case v1.MethodPermissionList:
		var params v1.PermissionListParams
		if err := decode(req, &params); err != nil {
			return nil, err
		}
		return g.service.ListPermissions(ctx, principal, params)

	case v1.MethodCommandGet:
		var params v1.CommandGetParams
		if err := decode(req, &params); err != nil {
			return nil, err
		}
		return g.service.GetCommand(ctx, principal, params.CommandID)

	case v1.MethodAgentList:
		return g.service.ListAgents(ctx, principal)

	case v1.MethodNodeList:
		return g.service.ListNodes(ctx, principal)

	default:
		return nil, v1.MethodNotFound(req.Method)
	}
}

func decode(req *v1.Message, target any) error {
	if len(req.Params) == 0 {
		return nil
	}
	if err := json.Unmarshal(req.Params, target); err != nil {
		return v1.InvalidParams("invalid %s request", req.Method)
	}
	return nil
}

func actorOf(req *v1.Message) string {
	if req.Meta == nil {
		return ""
	}
	return req.Meta.Actor
}
