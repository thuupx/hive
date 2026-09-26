package control

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/thupham/hive/internal/permission"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// RespondToPermission relays a permission decision to the agent, then records it.
//
// The relay comes first: recording a decision the agent never received would leave
// the request pending while the coordinator believed it was resolved. A failed
// relay leaves the request pending, which fails closed.
//
// A pending request never becomes an implicit approval. If nobody answers, it
// expires into a safe terminal state.
func (s *Service) RespondToPermission(ctx context.Context, principal Principal, params v1.PermissionRespondParams) error {
	if err := s.policy.Authorize(principal, v1.MethodPermissionRespond); err != nil {
		return err
	}
	if params.AgentRequestID == "" {
		return v1.InvalidParams("agentRequestId is required")
	}

	request, err := s.findOpenPermission(ctx, params)
	if err != nil {
		return err
	}
	if err := s.authorizeSession(ctx, principal, request.SessionID); err != nil {
		return err
	}

	run, err := s.store.GetAgentRun(ctx, request.RunID)
	if err != nil {
		return storageError(err)
	}

	executionNode, ok := s.nodes.Node(run.NodeID)
	if !ok || !executionNode.Connected() {
		return v1.Unavailable("node %s is not connected", run.NodeID)
	}

	// The agent's own request id is relayed verbatim, because it is the id the
	// agent is waiting on. A caller may have typed it without the JSON quoting the
	// protocol uses, which is why the lookup normalizes.
	err = executionNode.Call(ctx, v1.MethodPermissionRespond, v1.PermissionRespondParams{
		AgentRunID:     run.ID,
		AgentID:        run.AgentID,
		AgentRequestID: request.AgentRequestID,
		Approved:       params.Approved,
		OptionID:       params.OptionID,
	}, nil)
	if err != nil {
		return err
	}

	// Only the first valid terminal transition wins, so a repeated response
	// observes the resolved state instead of authorizing twice.
	_, _, err = s.store.ResolvePermissionRequest(ctx, request.ID, decisionState(request, params))
	return err
}

// decisionState is how a decision is recorded.
//
// An agent's option ids and kinds are its own: "allow_once" is an approval even
// though it is not called "allow", and recording a denial for it would make the
// audit trail say the opposite of what the user chose. So when the response names
// one of the agent's options, the state follows that option's kind, and the
// transport's approval flag is used only when there is no option to read.
func decisionState(request *permission.Request, params v1.PermissionRespondParams) permission.State {
	if params.Approved {
		return permission.StateApproved
	}
	if params.OptionID != "" && strings.HasPrefix(optionKind(request.Payload, params.OptionID), "allow") {
		return permission.StateApproved
	}
	return permission.StateDenied
}

// optionKind is the kind of the option an agent offered, found by its id.
func optionKind(payload json.RawMessage, optionID string) string {
	var request struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		return ""
	}
	for _, option := range request.Options {
		if option.OptionID == optionID {
			return option.Kind
		}
	}
	return ""
}

// findOpenPermission locates a pending request by the agent's own request id.
func (s *Service) findOpenPermission(ctx context.Context, params v1.PermissionRespondParams) (*permission.Request, error) {
	if params.SessionID != "" {
		open, err := s.store.ListOpenPermissionRequests(ctx, params.SessionID)
		if err != nil {
			return nil, storageError(err)
		}
		for _, request := range open {
			if sameRequestID(request.AgentRequestID, params.AgentRequestID) {
				return request, nil
			}
		}
		return nil, v1.NotFound("permission request %s", params.AgentRequestID)
	}

	open, err := s.store.ListAllOpenPermissionRequests(ctx, 500)
	if err != nil {
		return nil, storageError(err)
	}
	for _, request := range open {
		if sameRequestID(request.AgentRequestID, params.AgentRequestID) {
			return request, nil
		}
	}
	return nil, v1.NotFound("permission request %s", params.AgentRequestID)
}

// sameRequestID compares an agent request id ignoring JSON quoting.
//
// A JSON-RPC id is a JSON value, so a string id is stored quoted. A caller types
// the id without quoting, and both forms name the same request.
func sameRequestID(stored, requested string) bool {
	return UnquoteRequestID(stored) == UnquoteRequestID(requested)
}

// UnquoteRequestID renders an agent request id the way a human would type it.
func UnquoteRequestID(id string) string {
	trimmed := strings.TrimSpace(id)
	if len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"' {
		var decoded string
		if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
			return decoded
		}
	}
	return trimmed
}
