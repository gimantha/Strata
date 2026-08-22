package domain

import "time"

// Scope is the resolved tenancy of an operation. It is produced from authenticated
// identity, never taken from a request body, and is mandatory on every durable
// record and every query (AGENTS.md sections 2.6, 22.1).
type Scope struct {
	WorkspaceID  WorkspaceID
	GraphSpaceID GraphSpaceID
	CollectionID CollectionID
}

// Workspace is the hard tenant and security boundary.
type Workspace struct {
	ID        WorkspaceID
	Slug      string
	Name      string
	Metadata  map[string]any
	CreatedAt time.Time
	UpdatedAt time.Time
}

// GraphSpace is one logically coherent context graph inside a workspace: a user, an
// organization, a project, or a shared agent memory domain (AGENTS.md section 6.1).
type GraphSpace struct {
	ID          GraphSpaceID
	WorkspaceID WorkspaceID
	Slug        string
	Name        string
	Metadata    map[string]any
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Collection groups source material within a graph space for retrieval and policy
// partitioning.
type Collection struct {
	ID           CollectionID
	WorkspaceID  WorkspaceID
	GraphSpaceID GraphSpaceID
	Slug         string
	Name         string
	Metadata     map[string]any
	CreatedAt    time.Time
}

// Principal is an authenticated actor: a human, an agent, or a service.
type Principal struct {
	ID          PrincipalID
	Kind        PrincipalKind
	DisplayName string
	// SystemRole gates workspace-creation-level operations that exist outside any
	// single workspace. Workspace access itself always comes from a grant.
	SystemRole Role
	Grants     []Grant
}

// Ref reduces a principal to the reference stored on durable records.
func (p Principal) Ref() PrincipalRef {
	return PrincipalRef{ID: p.ID, Kind: p.Kind, DisplayName: p.DisplayName}
}

// GrantFor returns the principal's role in a workspace, if any.
func (p Principal) GrantFor(ws WorkspaceID) (Role, bool) {
	for _, g := range p.Grants {
		if g.WorkspaceID == ws {
			return g.Role, true
		}
	}
	return "", false
}

// PrincipalRef is the denormalized actor reference persisted on records that need
// to record who caused them.
type PrincipalRef struct {
	ID          PrincipalID
	Kind        PrincipalKind
	DisplayName string
}

// Grant binds a principal to a workspace with a role.
type Grant struct {
	PrincipalID PrincipalID
	WorkspaceID WorkspaceID
	Role        Role
	CreatedAt   time.Time
}

func validateSlug(op, field, slug string) error {
	if slug == "" {
		return Errorf(CodeInvalidArgument, op, "%s is required", field)
	}
	if len(slug) > 128 {
		return Errorf(CodeInvalidArgument, op, "%s must be at most 128 characters", field)
	}
	for _, r := range slug {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return Errorf(CodeInvalidArgument, op,
				"%s must contain only lowercase letters, digits, hyphen, or underscore", field)
		}
	}
	return nil
}

func (w Workspace) Validate() error {
	const op = "domain.Workspace.Validate"
	if err := validateSlug(op, "slug", w.Slug); err != nil {
		return err
	}
	if w.Name == "" {
		return Errorf(CodeInvalidArgument, op, "name is required")
	}
	return nil
}

func (g GraphSpace) Validate() error {
	const op = "domain.GraphSpace.Validate"
	if IsZero(g.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id is required")
	}
	if err := validateSlug(op, "slug", g.Slug); err != nil {
		return err
	}
	if g.Name == "" {
		return Errorf(CodeInvalidArgument, op, "name is required")
	}
	return nil
}

func (c Collection) Validate() error {
	const op = "domain.Collection.Validate"
	if IsZero(c.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id is required")
	}
	if IsZero(c.GraphSpaceID) {
		return Errorf(CodeInvalidArgument, op, "graph_space_id is required")
	}
	return validateSlug(op, "slug", c.Slug)
}
