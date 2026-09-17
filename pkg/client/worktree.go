package client

import (
	"context"

	"github.com/aiomni/dune/pkg/api"
)

func (c *Client) Worktrees(ctx context.Context, directory string) ([]api.Worktree, error) {
	var result []api.Worktree
	err := c.Call(ctx, "worktree.list", map[string]string{"directory": directory}, &result)
	return result, err
}

func (c *Client) CreateWorktree(ctx context.Context, request api.WorktreeCreate) (api.Worktree, error) {
	var result api.Worktree
	err := c.Call(ctx, "worktree.create", request, &result)
	return result, err
}
