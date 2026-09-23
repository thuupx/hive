// Package client is a Control API client.
//
// The CLI and the TUI are clients of Hive APIs, not independent orchestration
// logic: everything they do goes through this package. Nothing here decides
// session semantics, because the coordinator owns them.
package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// DialTimeout bounds a connection attempt.
const DialTimeout = 5 * time.Second

// Client is a Control API connection.
type Client struct {
	peer *v1.Peer
	conn net.Conn
}

// Dial connects to the coordinator's Control API socket.
func Dial(ctx context.Context, socketPath string) (*Client, error) {
	if socketPath == "" {
		return nil, errors.New("client: a socket path is required")
	}

	ctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("client: connect to %s: %w", socketPath, err)
	}

	peer := v1.NewPeer(v1.NewStream(conn, conn, conn))
	peer.Start()

	return &Client{peer: peer, conn: conn}, nil
}

// Close releases the connection.
func (c *Client) Close() error {
	c.peer.Close()
	return c.conn.Close()
}

// CreateSession creates a session and its first AgentRun.
func (c *Client) CreateSession(ctx context.Context, params v1.SessionCreateParams) (*v1.SessionCreateResult, error) {
	var result v1.SessionCreateResult
	if err := c.peer.Call(ctx, v1.MethodSessionCreate, params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Prompt sends a prompt to a session.
func (c *Client) Prompt(ctx context.Context, params v1.SessionPromptParams) (*v1.SessionPromptResult, error) {
	var result v1.SessionPromptResult
	if err := c.peer.Call(ctx, v1.MethodSessionPrompt, params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Handoff transfers a session to another agent.
func (c *Client) Handoff(ctx context.Context, params v1.SessionHandoffParams) (*v1.SessionHandoffResult, error) {
	var result v1.SessionHandoffResult
	if err := c.peer.Call(ctx, v1.MethodSessionHandoff, params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Cancel cancels the current turn of a run.
func (c *Client) Cancel(ctx context.Context, params v1.SessionCancelParams) error {
	return c.peer.Call(ctx, v1.MethodSessionCancel, params, nil)
}

// Status returns one session and its runs.
func (c *Client) Status(ctx context.Context, sessionID string) (*v1.SessionStatusResult, error) {
	var result v1.SessionStatusResult
	if err := c.peer.Call(ctx, v1.MethodSessionStatus, v1.SessionStatusParams{SessionID: sessionID}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListSessions returns the sessions this client may see.
func (c *Client) ListSessions(ctx context.Context, limit int) (*v1.SessionListResult, error) {
	var result v1.SessionListResult
	if err := c.peer.Call(ctx, v1.MethodSessionList, v1.SessionListParams{Limit: limit}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Replay returns a session event stream from a cursor.
func (c *Client) Replay(ctx context.Context, params v1.EventReplayParams) (*v1.EventReplayResult, error) {
	var result v1.EventReplayResult
	if err := c.peer.Call(ctx, v1.MethodSessionEvents, params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetCommand returns a command status resource.
func (c *Client) GetCommand(ctx context.Context, commandID string) (*v1.CommandResult, error) {
	var result v1.CommandResult
	if err := c.peer.Call(ctx, v1.MethodCommandGet, v1.CommandGetParams{CommandID: commandID}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CreateWorkspace registers a workspace.
func (c *Client) CreateWorkspace(ctx context.Context, params v1.WorkspaceCreateParams) (*v1.WorkspaceSummary, error) {
	var result v1.WorkspaceSummary
	if err := c.peer.Call(ctx, v1.MethodWorkspaceCreate, params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListWorkspaces returns the workspaces and the shared-location warnings.
func (c *Client) ListWorkspaces(ctx context.Context) (*v1.WorkspaceListResult, error) {
	var result v1.WorkspaceListResult
	if err := c.peer.Call(ctx, v1.MethodWorkspaceList, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListPermissions returns pending permission requests.
func (c *Client) ListPermissions(ctx context.Context, params v1.PermissionListParams) (*v1.PermissionListResult, error) {
	var result v1.PermissionListResult
	if err := c.peer.Call(ctx, v1.MethodPermissionList, params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// RespondToPermission answers a pending permission request.
func (c *Client) RespondToPermission(ctx context.Context, params v1.PermissionRespondParams) error {
	return c.peer.Call(ctx, v1.MethodPermissionRespond, params, nil)
}

// ListAgents returns the configured agents.
func (c *Client) ListAgents(ctx context.Context) (*v1.AgentListResult, error) {
	var result v1.AgentListResult
	if err := c.peer.Call(ctx, v1.MethodAgentList, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListNodes returns the nodes the coordinator has heard from.
func (c *Client) ListNodes(ctx context.Context) (*v1.NodeListResult, error) {
	var result v1.NodeListResult
	if err := c.peer.Call(ctx, v1.MethodNodeList, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
