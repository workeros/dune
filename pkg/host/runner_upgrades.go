package host

import (
	"context"
	"errors"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
)

type runnerUpgrades struct{ app *App }

func (a *App) RunnerUpgrader() upgrade.Service { return &runnerUpgrades{app: a} }

func (u *runnerUpgrades) begin(ctx context.Context, scope upgrade.Scope, operation string) (context.Context, func(), error) {
	ctx, finish, err := u.app.adminContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	duration := 20 * time.Second
	if operation == "runner.upgrade.preview" {
		duration = 4 * time.Minute
	}
	bounded, cancel := context.WithTimeout(ctx, duration)
	done := func() { cancel(); finish() }
	resource, _, err := u.app.authorizer.Resource(bounded, scope.Principal, scope.Binding.MachineID, true, operation)
	if err == nil && (resource.OwnerID != scope.OwnerID || resource.Runner.Binding == nil || *resource.Runner.Binding != scope.Binding) {
		err = runner.ErrBindingChanged
	}
	if err != nil {
		done()
		return nil, nil, err
	}
	return bounded, done, nil
}
func (u *runnerUpgrades) call(ctx context.Context, scope upgrade.Scope, operation string, input, output any) error {
	connection, close, err := (&runnerExecutor{app: u.app}).connect(ctx, scope.Principal, scope.OwnerID, scope.Binding, operation)
	if err != nil {
		return err
	}
	defer close()
	return connection.Call(ctx, operation, input, output)
}
func (u *runnerUpgrades) InspectRunner(ctx context.Context, scope upgrade.Scope) (upgrade.Inspection, error) {
	var result upgrade.Inspection
	ctx, finish, err := u.begin(ctx, scope, "runner.upgrade.inspect")
	if err != nil {
		return result, err
	}
	defer finish()
	err = u.call(ctx, scope, "runner.upgrade.inspect", scope.Binding, &result)
	return result, err
}
func (u *runnerUpgrades) PreviewUpgrade(ctx context.Context, scope upgrade.Scope, request upgrade.PreviewRequest) (upgrade.Preview, error) {
	var result upgrade.Preview
	if request.Binding != scope.Binding {
		return result, runner.ErrBindingChanged
	}
	ctx, finish, err := u.begin(ctx, scope, "runner.upgrade.preview")
	if err != nil {
		return result, err
	}
	defer finish()
	err = u.call(ctx, scope, "runner.upgrade.preview", request, &result)
	return result, err
}
func (u *runnerUpgrades) StartUpgrade(ctx context.Context, scope upgrade.Scope, request upgrade.Request) (upgrade.Observation, error) {
	unknown := upgrade.Observation{Operation: upgrade.Operation{Request: request, Admission: api.SubmissionUnknown}, Freshness: "unreachable"}
	if request.Binding != scope.Binding || request.Validate() != nil {
		return unknown, &api.Error{Code: "INVALID_ARGUMENT", Detail: "complete original request must match authorized binding"}
	}
	ctx, finish, err := u.begin(ctx, scope, "runner.upgrade.start")
	if err != nil {
		return unknown, err
	}
	defer finish()
	original, created, err := u.app.upgradeObservations.Reserve(ctx, scope.OwnerID, request)
	if err != nil {
		return unknown, err
	}
	if !created {
		// Once dispatch may have happened, even a host restart can only query this
		// key. An unknown record never authorizes another upgrade submission.
		return u.GetUpgrade(ctx, scope, upgrade.Query{Binding: request.Binding, InstallationID: request.InstallationID, SubmissionID: request.SubmissionID})
	}
	op := original.Operation
	submission := upgrade.Submission{Request: request, ReservedAt: original.Operation.StartedAt}
	if err := u.call(ctx, scope, "runner.upgrade.start", submission, &op); err != nil {
		original.Freshness = "unreachable"
		return original, err
	}
	observed := upgrade.Observation{Operation: op, Freshness: "live", ObservedAt: time.Now().UTC()}
	if err := u.app.upgradeObservations.Observe(ctx, scope.OwnerID, observed); err != nil {
		return observed, err
	}
	return observed, nil
}
func (u *runnerUpgrades) GetUpgrade(ctx context.Context, scope upgrade.Scope, query upgrade.Query) (upgrade.Observation, error) {
	if query.Binding != scope.Binding || query.Validate() != nil {
		return upgrade.Observation{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "original upgrade selector required"}
	}
	ctx, finish, err := u.begin(ctx, scope, "runner.upgrade.get")
	if err != nil {
		return upgrade.Observation{}, err
	}
	defer finish()
	var op upgrade.Operation
	callErr := u.call(ctx, scope, "runner.upgrade.get", query, &op)
	if callErr == nil {
		observed := upgrade.Observation{Operation: op, Freshness: "live", ObservedAt: time.Now().UTC()}
		if err := u.app.upgradeObservations.Observe(ctx, scope.OwnerID, observed); err != nil {
			return observed, err
		}
		return observed, nil
	}
	if err := u.authorize(ctx, scope, "runner.upgrade.get"); err != nil {
		return upgrade.Observation{}, err
	}
	observed, err := u.app.upgradeObservations.Observation(ctx, scope.OwnerID, query)
	if err == nil {
		observed.Freshness = "last_known"
		observed.ObservationIssue = observationIssue(callErr)
		return observed, nil
	}
	if !errors.Is(err, upgrade.ErrObservationNotFound) {
		return observed, err
	}
	return upgrade.Observation{Operation: upgrade.Operation{Request: upgrade.Request{Binding: query.Binding, InstallationID: query.InstallationID, SubmissionID: query.SubmissionID}, Admission: api.SubmissionUnknown}, Freshness: "unreachable"}, callErr
}
func (u *runnerUpgrades) ListUpgrades(ctx context.Context, scope upgrade.Scope, request upgrade.ListRequest) (upgrade.History, error) {
	if request.Binding != scope.Binding || request.Validate() != nil {
		return upgrade.History{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "original upgrade history scope required"}
	}
	ctx, finish, err := u.begin(ctx, scope, "runner.upgrade.list")
	if err != nil {
		return upgrade.History{}, err
	}
	defer finish()
	// Freeze unknown submissions before reading the executor. A receipt arriving
	// during that read must not remove a key from both sources of this page.
	// MergePages prefers any executor facts over this earlier observation.
	unknown, err := u.app.upgradeObservations.UnknownSubmissions(ctx, scope.OwnerID, request)
	if err != nil {
		return upgrade.History{}, err
	}
	var page upgrade.Page
	if err := u.call(ctx, scope, "runner.upgrade.list", request, &page); err != nil {
		if authErr := u.authorize(ctx, scope, "runner.upgrade.list"); authErr != nil {
			return upgrade.History{}, authErr
		}
		known, storeErr := u.app.upgradeObservations.Observations(ctx, scope.OwnerID, request)
		known.ObservationIssue = observationIssue(err)
		if storeErr != nil {
			return known, storeErr
		}
		if known.ObservedAt.IsZero() {
			known.Freshness = "unreachable"
		}
		return known, nil
	}
	now := time.Now().UTC()
	operations := append([]upgrade.Operation(nil), page.Items...)
	if page.Active != nil {
		operations = append(operations, *page.Active)
	}
	for _, op := range operations {
		if err := u.app.upgradeObservations.Observe(ctx, scope.OwnerID, upgrade.Observation{Operation: op, Freshness: "live", ObservedAt: now}); err != nil {
			return upgrade.History{Page: page, Freshness: "live", ObservedAt: now}, err
		}
	}
	merged, err := upgrade.MergePages(page, unknown, request.Cursor, request.Limit)
	return upgrade.History{Page: merged, Freshness: "live", ObservedAt: now}, err
}

func (u *runnerUpgrades) authorize(ctx context.Context, scope upgrade.Scope, operation string) error {
	resource, _, err := u.app.authorizer.Resource(ctx, scope.Principal, scope.Binding.MachineID, true, operation)
	if err != nil {
		return err
	}
	if resource.OwnerID != scope.OwnerID || resource.Runner.Binding == nil || *resource.Runner.Binding != scope.Binding {
		return runner.ErrBindingChanged
	}
	return nil
}
func observationIssue(err error) *upgrade.Issue {
	code := "RUNNER_UNREACHABLE"
	var protocol *api.Error
	if errors.As(err, &protocol) && api.ValidateSubmissionID(protocol.Code) == nil {
		code = protocol.Code
	}
	return &upgrade.Issue{Code: code, Stage: "observation"}
}
