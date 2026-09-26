package control

import (
	"context"

	"github.com/thuupx/hive/internal/command"
	"github.com/thuupx/hive/internal/ids"
	"github.com/thuupx/hive/internal/storage"
	"github.com/thuupx/hive/internal/workspace"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// CreateWorkspace registers a workspace and its node-local locations.
//
// The command record and the workspace commit together, so a retry cannot create a
// second workspace with the same name.
func (s *Service) CreateWorkspace(ctx context.Context, principal Principal, params v1.WorkspaceCreateParams) (*v1.WorkspaceSummary, error) {
	if err := s.policy.Authorize(principal, v1.MethodWorkspaceCreate); err != nil {
		return nil, err
	}
	switch {
	case params.CommandID == "":
		return nil, v1.InvalidParams("commandId is required")
	case params.Name == "":
		return nil, v1.InvalidParams("name is required")
	}

	for nodeID, path := range params.Locations {
		if nodeID == "" || path == "" {
			return nil, v1.InvalidParams("a workspace location needs a node and a path")
		}
	}

	// A retry of the same operation returns the existing workspace.
	if existing, err := s.store.GetWorkspaceByName(ctx, params.Name); err == nil {
		return s.workspaceSummary(ctx, existing)
	}

	record := workspace.New(ids.New("ws"), params.Name)

	cmd := command.New(params.CommandID, string(principal), v1.MethodWorkspaceCreate)
	cmd.State = command.StateAccepted
	cmd.Target = record.ID

	stored, created, err := s.store.AcceptCommandWith(ctx, cmd, func(tx storage.Execer) error {
		if err := s.store.InsertWorkspace(ctx, tx, record); err != nil {
			return err
		}
		for nodeID, path := range params.Locations {
			if err := s.store.PutWorkspaceLocation(ctx, tx, workspace.Location{
				WorkspaceID: record.ID,
				NodeID:      nodeID,
				Path:        path,
			}); err != nil {
				return err
			}
		}
		return nil
	}, nil)
	if err != nil {
		return nil, domainError(err)
	}

	if !created {
		// The name already exists, which the pre-check above handles; reaching
		// here means a concurrent creation won.
		existing, err := s.store.GetWorkspaceByName(ctx, params.Name)
		if err != nil {
			return nil, storageError(err)
		}
		return s.workspaceSummary(ctx, existing)
	}

	summary, err := s.workspaceSummary(ctx, record)
	if err != nil {
		return nil, err
	}
	if err := s.completeCommand(ctx, stored.ID, summary); err != nil {
		s.log.Warn("command result could not be recorded", "command", stored.ID, "error", err)
	}
	return summary, nil
}

// ListWorkspaces returns the workspaces and the shared-location warnings.
func (s *Service) ListWorkspaces(ctx context.Context, principal Principal) (*v1.WorkspaceListResult, error) {
	if err := s.policy.Authorize(principal, v1.MethodWorkspaceList); err != nil {
		return nil, err
	}

	records, err := s.store.ListWorkspaces(ctx)
	if err != nil {
		return nil, storageError(err)
	}

	usages, err := s.store.WorkspaceUsage(ctx)
	if err != nil {
		return nil, storageError(err)
	}

	active := map[string]int{}
	for _, usage := range usages {
		active[usage.WorkspaceID] += len(usage.RunIDs)
	}

	result := &v1.WorkspaceListResult{Workspaces: make([]v1.WorkspaceSummary, 0, len(records))}
	for _, record := range records {
		summary, err := s.workspaceSummary(ctx, record)
		if err != nil {
			return nil, err
		}
		summary.ActiveRuns = active[record.ID]
		result.Workspaces = append(result.Workspaces, *summary)
	}

	// A shared location is a warning, never a lock.
	for _, warning := range workspace.Warnings(usages) {
		result.Warnings = append(result.Warnings, v1.WorkspaceWarning{
			WorkspaceID: warning.WorkspaceID,
			NodeID:      warning.NodeID,
			Path:        warning.Path,
			RunIDs:      warning.RunIDs,
		})
	}
	return result, nil
}

func (s *Service) workspaceSummary(ctx context.Context, record *workspace.Workspace) (*v1.WorkspaceSummary, error) {
	locations, err := s.store.ListWorkspaceLocations(ctx, record.ID)
	if err != nil {
		return nil, storageError(err)
	}

	summary := &v1.WorkspaceSummary{
		WorkspaceID: record.ID,
		Name:        record.Name,
	}
	if len(locations) > 0 {
		summary.Locations = make(map[string]string, len(locations))
		for _, loc := range locations {
			summary.Locations[loc.NodeID] = loc.Path
		}
	}
	return summary, nil
}

// resolveWorkspace maps a workspace name to its identifier, creating the workspace
// when it does not exist yet.
//
// Naming a workspace on a session is enough: the caller does not have to register
// it first, and the node-local path is filled in when the node is known.
func (s *Service) resolveWorkspace(ctx context.Context, name, nodeID, path string) (string, error) {
	if name == "" {
		return "", nil
	}

	existing, err := s.store.GetWorkspaceByName(ctx, name)
	if err == nil {
		if nodeID != "" && path != "" {
			if err := s.store.WriteTx(ctx, func(tx storage.Execer) error {
				return s.store.PutWorkspaceLocation(ctx, tx, workspace.Location{
					WorkspaceID: existing.ID,
					NodeID:      nodeID,
					Path:        path,
				})
			}); err != nil {
				return "", err
			}
		}
		return existing.ID, nil
	}

	record := workspace.New(ids.New("ws"), name)
	if err := s.store.WriteTx(ctx, func(tx storage.Execer) error {
		if err := s.store.InsertWorkspace(ctx, tx, record); err != nil {
			return err
		}
		if nodeID == "" || path == "" {
			return nil
		}
		return s.store.PutWorkspaceLocation(ctx, tx, workspace.Location{
			WorkspaceID: record.ID,
			NodeID:      nodeID,
			Path:        path,
		})
	}); err != nil {
		return "", err
	}
	return record.ID, nil
}
