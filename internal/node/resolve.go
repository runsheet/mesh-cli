package node

import (
	"context"
	"fmt"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Resolved is a work item with its goal, project and repo read.
type Resolved struct {
	Item    gateway.WorkItem
	Goal    *gateway.Goal
	Project *gateway.Project
	Repo    *gateway.Repo
}

// Branch is the item's branch name.
func (r *Resolved) Branch() string { return BranchName(r.Goal.Slug, r.Item.Seq) }

// BaseRef is the item's base ref, or the repo's default branch.
func (r *Resolved) BaseRef() string {
	if r.Item.BaseRef != "" {
		return r.Item.BaseRef
	}
	if r.Repo != nil && r.Repo.DefaultBranch != "" {
		return r.Repo.DefaultBranch
	}
	return "main"
}

// Resolve reads the item's goal, project and repo.
func Resolve(ctx context.Context, gw Gateway, item gateway.WorkItem) (*Resolved, error) {
	goal, err := gw.Goal(ctx, item.GoalID)
	if err != nil {
		return nil, fmt.Errorf("goal: %w", err)
	}
	project, err := gw.Project(ctx, goal.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("project: %w", err)
	}
	repo, err := gw.Repo(ctx, item.RepoID)
	if err != nil {
		return nil, fmt.Errorf("repo: %w", err)
	}
	return &Resolved{Item: item, Goal: goal, Project: project, Repo: repo}, nil
}
