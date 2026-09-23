package client

import (
	"context"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
	"slices"
)

func (c *Client) InspectRunner(ctx context.Context, binding runner.Binding) (upgrade.Inspection, error) {
	var result upgrade.Inspection
	if !slices.Contains(c.Binding.Capabilities, "runner.upgrade.inspect") {
		return result, &api.Error{Code: "UPGRADE_UNSUPPORTED", Detail: "Runner does not advertise online upgrade inspect"}
	}
	err := c.Call(ctx, "runner.upgrade.inspect", binding, &result)
	return result, err
}
func (c *Client) PreviewUpgrade(ctx context.Context, request upgrade.PreviewRequest) (upgrade.Preview, error) {
	var result upgrade.Preview
	if !slices.Contains(c.Binding.Capabilities, "runner.upgrade.preview") {
		return result, &api.Error{Code: "UPGRADE_UNSUPPORTED", Detail: "Runner does not advertise online upgrade preview"}
	}
	err := c.Call(ctx, "runner.upgrade.preview", request, &result)
	return result, err
}

// StartUpgrade sends exactly once. An interrupted response leaves admission
// unknown; callers durably reserve the submission before dispatch and query
// instead of replaying. Application callers should use upgrade.Service.
func (c *Client) StartUpgrade(ctx context.Context, submission upgrade.Submission) (upgrade.Operation, error) {
	result := upgrade.Operation{Request: submission.Request, Admission: api.SubmissionUnknown}
	if err := submission.Validate(); err != nil {
		return result, err
	}
	if !slices.Contains(c.Binding.Capabilities, "runner.upgrade.start") {
		return result, &api.Error{Code: "UPGRADE_UNSUPPORTED", Detail: "Runner does not advertise online upgrade start"}
	}
	err := c.Call(ctx, "runner.upgrade.start", submission, &result)
	return result, err
}
func (c *Client) GetUpgrade(ctx context.Context, query upgrade.Query) (upgrade.Operation, error) {
	result := upgrade.Operation{Request: upgrade.Request{Binding: query.Binding, InstallationID: query.InstallationID, SubmissionID: query.SubmissionID}, Admission: api.SubmissionUnknown}
	if !slices.Contains(c.Binding.Capabilities, "runner.upgrade.get") {
		return result, &api.Error{Code: "UPGRADE_UNSUPPORTED", Detail: "Runner does not advertise online upgrade get"}
	}
	err := c.Call(ctx, "runner.upgrade.get", query, &result)
	return result, err
}
func (c *Client) ListUpgrades(ctx context.Context, request upgrade.ListRequest) (upgrade.Page, error) {
	var result upgrade.Page
	if !slices.Contains(c.Binding.Capabilities, "runner.upgrade.list") {
		return result, &api.Error{Code: "UPGRADE_UNSUPPORTED", Detail: "Runner does not advertise online upgrade list"}
	}
	err := c.Call(ctx, "runner.upgrade.list", request, &result)
	return result, err
}
