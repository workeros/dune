package fabricd

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func (d *Engine) start(s *executionStream, message *pb.Message) {
	var request api.StartRequest
	if wire.Decode(message, &request) != nil || request.SubmissionKey.Validate() != nil || request.Target.RuntimeID != "" || !matchesSubmissionTarget(request.Target, message, message.Target) {
		s.Fail("INVALID_ARGUMENT", fmt.Errorf("complete caller-owned launch key required"))
		return
	}
	key := request.SubmissionKey
	receipt := api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	select {
	case d.starts <- struct{}{}:
		defer func() { <-d.starts }()
	default:
		failSubmission(s, receipt, &api.Error{Code: "SUBMISSION_CAPACITY_EXHAUSTED", Detail: "launch concurrency capacity reached"})
		return
	}
	if s.Send(&pb.Message{Kind: "accepted", RequestId: message.RequestId}) != nil {
		return
	}
	gate, gateErr := launchgate.Acquire(d.stateDir, false)
	releaseGate := func() {
		if gate != nil {
			gate.Close()
			gate = nil
		}
	}
	defer releaseGate()
	claim, receipt, err := d.registry.ClaimKey(s.ctx, key, sessionregistry.Digest("profile.start", api.Payload(request)), "registry:launch")
	if err != nil {
		failSubmission(s, receipt, err)
		return
	}
	if !claim.Acquired() {
		code := "ALREADY_SUBMITTED"
		if receipt.Admission == api.SubmissionUnknown {
			code = "RESULT_UNKNOWN"
		} else if receipt.Admission == api.SubmissionNotAccepted {
			code = receipt.ErrorCode
		}
		failSubmission(s, receipt, &api.Error{Code: code, Detail: "original launch already has admission evidence; query its original key"})
		return
	}
	if gateErr != nil {
		code := "LAUNCH_GATE_UNAVAILABLE"
		if errors.Is(gateErr, launchgate.ErrBusy) {
			code = "UPGRADE_IN_PROGRESS"
		}
		// Only the durable rejection seals this key. If persistence fails, keep
		// unknown; neither this request nor a later duplicate may execute it.
		receipt, err = d.registry.Reject(d.ctx, claim, code)
		if err == nil {
			err = &api.Error{Code: code, Detail: "new Runtime admission is gated by connector replacement"}
		}
		failSubmission(s, receipt, err)
		return
	}
	p := request.Profile
	validation := p.Validate()
	if p.Kind != "agent" {
		validation = fmt.Errorf("profile.start requires kind: agent")
	}
	if request.Worktree != nil && (request.Worktree.Directory != p.WorkingDirectory || !filepath.IsAbs(request.Worktree.Path)) {
		validation = fmt.Errorf("worktree requires the frozen source directory and an absolute destination")
	}
	if validation != nil {
		receipt, err = d.registry.Reject(d.ctx, claim, "INVALID_ARGUMENT")
		if err == nil {
			err = &api.Error{Code: "INVALID_ARGUMENT", Detail: validation.Error()}
		}
		failSubmission(s, receipt, err)
		return
	}
	argv, _ := p.Start.Args()
	r := &runtime{id: wire.ID(), inc: wire.ID(), adapter: p.Adapter, title: filepath.Base(argv[0]), cwd: p.WorkingDirectory, projectID: p.ProjectID, directoryID: p.DirectoryID, subs: map[*subscription]bool{}, done: make(chan struct{}), conversations: d.conversations}
	r.observer = d.publishRuntime
	if p.Adapter == "acp" {
		r.hostInfo = &api.ACPHostInfo{Protocol: sessionProtocol}
	}
	reserved := r.info()
	reserved.State = "starting"
	receipt, err = d.registry.AcceptLaunch(d.ctx, claim, wire.ID(), reserved)
	if err != nil {
		var failure *api.Error
		if errors.As(err, &failure) {
			if rejected, rejectErr := d.registry.Reject(d.ctx, claim, failure.Code); rejectErr == nil {
				receipt = rejected
			}
		}
		failSubmission(s, receipt, err)
		return
	}
	d.mu.Lock()
	d.launching[r.id] = r.inc
	d.mu.Unlock()
	d.recordLifecycle("launch_accepted", r, receipt.OperationRef, "", d.sessionTerm)
	defer func() { d.mu.Lock(); delete(d.launching, r.id); d.mu.Unlock() }()
	progress := func(stage, code string, runtime *api.Runtime) error {
		observed, err := d.registry.Progress(d.ctx, key, receipt.OperationRef, stage, code, runtime)
		if err == nil {
			receipt = observed
			_ = s.Send(&pb.Message{Kind: "progress", Payload: api.Payload(receipt)})
		}
		return err
	}
	fail := func(code string, cause error) {
		if err := progress("failed", code, nil); err != nil {
			failSubmission(s, receipt, err)
			return
		}
		failSubmission(s, receipt, &api.Error{Code: code, Detail: cause.Error()})
	}
	if request.Worktree != nil {
		if err := progress("worktree", "", nil); err != nil {
			failSubmission(s, receipt, err)
			return
		}
		created, err := d.createWorktree(*request.Worktree)
		if err != nil {
			fail("WORKTREE_FAILED", err)
			return
		}
		receipt, err = d.registry.RecordWorktree(d.ctx, key, receipt.OperationRef, created)
		if err != nil {
			failSubmission(s, receipt, err)
			return
		}
		p.WorkingDirectory, p.DirectoryID = created.Path, ""
		r.cwd, r.directoryID = created.Path, ""
	}
	if len(p.Setup.Steps) > 0 {
		if err := progress("setup", "", nil); err != nil {
			failSubmission(s, receipt, err)
			return
		}
	}
	for i, step := range p.Setup.Steps {
		result, err := d.exec(api.Exec{Command: step, WorkingDirectory: p.WorkingDirectory, Env: p.Env})
		if err != nil || result.ExitCode != 0 || result.TimedOut {
			_, failure := profileSetupFailure(message.RequestId, p.Kind, i, step.Name, result, err)
			if err := progress("failed", failure.Code, nil); err != nil {
				failSubmission(s, receipt, err)
				return
			}
			_ = s.Send(&pb.Message{Kind: "error", Code: failure.Code, Detail: failure.Detail, Payload: api.Payload(api.StartResult{SubmissionReceipt: receipt, Failure: failure})})
			return
		}
	}
	if err := progress("host_starting", "", nil); err != nil {
		failSubmission(s, receipt, err)
		return
	}
	if err := d.createAgent(p, message.Target, r, receipt); err != nil {
		// An asynchronous host may have started despite a missing acknowledgement.
		// Preserve its independently recorded phase instead of recording failure.
		if observed, readErr := d.registry.Get(d.ctx, key); readErr == nil {
			receipt = observed
		}
		failSubmission(s, receipt, err)
		return
	}
	d.mu.Lock()
	d.runtimes[r.id] = r
	d.mu.Unlock()
	d.startRuntimeObservers()
	if r.host == nil {
		r.publishObservation()
	}
	info := r.info()
	if err := progress("started", "", &info); err != nil {
		failSubmission(s, receipt, err)
		return
	}
	// The attached observer can outlive this connector. It must not hold the
	// upgrade gate after the original host has been published.
	releaseGate()
	if r.host != nil {
		if s.Send(&pb.Message{Kind: "result", Payload: api.Payload(api.StartResult{SubmissionReceipt: receipt})}) != nil {
			return
		}
		r.host.mu.Lock()
		attach := r.host.request("runtime.attach", api.Attach{Observe: true})
		r.host.mu.Unlock()
		r.host.forward(s, attach, true)
		return
	}
	sub, err := r.subscribe(true, false)
	if err != nil {
		failSubmission(s, receipt, err)
		return
	}
	_, epoch := r.control.state(sub)
	if s.Send(&pb.Message{Kind: "result", Payload: api.Payload(api.StartResult{SubmissionReceipt: receipt}), ControlEpoch: epoch}) != nil {
		r.unsubscribe(sub)
		return
	}
	d.interact(s, r, sub)
}
