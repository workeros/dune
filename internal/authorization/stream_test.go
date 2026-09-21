package authorization

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/identity"
)

type streamSessions struct {
	authenticated identity.Authentication
	err           error
	calls         int
}

func (*streamSessions) Namespace() string { return "enterprise" }
func (s *streamSessions) Authenticate(context.Context, string) (identity.Authentication, error) {
	s.calls++
	return s.authenticated, s.err
}

type streamRepository struct {
	Repository
	valid bool
	err   error
	calls int
}

func (r *streamRepository) CheckRunnerAccess(context.Context, ConnectionAccess) (bool, error) {
	r.calls++
	return r.valid, r.err
}

type streamHostChecker func(context.Context, access.Request) (access.Decision, error)

func (f streamHostChecker) Check(ctx context.Context, r access.Request) (access.Decision, error) {
	return f(ctx, r)
}

func streamCheckFixture() (*Service, *streamSessions, *streamRepository, ConnectionAccess) {
	user := identity.User{ID: "person", Kind: "email", Namespace: "enterprise", Subject: "person@example.test"}
	sessions := &streamSessions{authenticated: identity.Authentication{User: user, ExpiresAt: time.Now().Add(time.Hour)}}
	repository := &streamRepository{valid: true}
	checker := streamHostChecker(func(_ context.Context, r access.Request) (access.Decision, error) {
		return access.Decision{Allowed: true, ID: r.RequestID, Reason: "HOST_ALLOW", ValidUntil: time.Now().Add(time.Minute)}, nil
	})
	service := New(context.Background(), sessions, repository, checker, nil)
	record := ConnectionAccess{Session: "cookie", PrincipalID: user.ID, PrincipalKind: user.Kind, Namespace: user.Namespace, Subject: user.Subject, Target: "machine", RunnerID: "runner", FabricID: "attached", BindingRevision: 1, OwnerID: "tenant"}
	return service, sessions, repository, record
}

func TestStreamCheckAlwaysReadsCurrentIdentityAndRunner(t *testing.T) {
	service, sessions, repository, record := streamCheckFixture()
	policy := service.streamPolicy(record)
	request := access.Request{Scope: record.Scope(), RequestID: "request", Operation: "runtime.list"}
	for range 2 {
		before := time.Now()
		decision, err := access.Evaluate(context.Background(), policy.Checker, request)
		if err != nil || decision.ValidUntil.After(before.Add(access.StreamLeaseLimit+10*time.Millisecond)) {
			t.Fatal("stream lease exceeded end-to-end bound", decision, err)
		}
	}
	if sessions.calls != 2 || repository.calls != 2 {
		t.Fatal("a new check reused identity or Runner state", sessions.calls, repository.calls)
	}
	repository.valid = false
	if _, err := access.Evaluate(context.Background(), policy.Checker, request); err == nil {
		t.Fatal("revoked Runner was accepted")
	}
}

func TestStreamCheckPreservesCredentialExpiryAndRejectsScopeMutation(t *testing.T) {
	service, sessions, _, record := streamCheckFixture()
	policy := service.streamPolicy(record)
	request := access.Request{Scope: record.Scope(), RequestID: "request", Operation: "runtime.list"}
	expires := time.Now().Add(300 * time.Millisecond)
	sessions.authenticated.ExpiresAt = expires
	decision, err := access.Evaluate(context.Background(), policy.Checker, request)
	if err != nil || decision.ValidUntil.After(expires) {
		t.Fatal("natural credential expiry was extended", decision, err)
	}
	sessions.authenticated.User.Subject = "someone-else@example.test"
	if _, err := access.Evaluate(context.Background(), policy.Checker, request); err == nil {
		t.Fatal("changed authenticated principal inherited an old scope")
	}
	request.Binding.Revision++
	if _, err := access.Evaluate(context.Background(), policy.Checker, request); err == nil {
		t.Fatal("changed Runner binding inherited an old scope")
	}
}

func TestStreamCheckSlowHostConsumesLeaseBudget(t *testing.T) {
	service, _, _, record := streamCheckFixture()
	service.checker = streamHostChecker(func(_ context.Context, request access.Request) (access.Decision, error) {
		time.Sleep(60 * time.Millisecond)
		return access.Decision{Allowed: true, ID: request.RequestID, Reason: "HOST_ALLOW", ValidUntil: time.Now().Add(time.Minute)}, nil
	})
	before := time.Now()
	decision, err := access.Evaluate(context.Background(), service.streamPolicy(record).Checker, access.Request{Scope: record.Scope(), RequestID: "slow", Operation: "runtime.list"})
	if err != nil || decision.ValidUntil.After(before.Add(access.StreamLeaseLimit+10*time.Millisecond)) {
		t.Fatal("host latency restarted the lease", decision, err)
	}
}

func TestStreamCheckFailureAndBackgroundBoundary(t *testing.T) {
	service, sessions, repository, record := streamCheckFixture()
	request := access.Request{Scope: record.Scope(), RequestID: "request", Operation: "runtime.list"}
	repository.err = errors.New("database unavailable")
	if _, err := access.Evaluate(context.Background(), service.streamPolicy(record).Checker, request); !errors.Is(err, access.ErrUnavailable) {
		t.Fatal("repository failure did not fail closed", err)
	}
	repository.err = nil
	record.Background, record.Session = true, ""
	sessions.err = identity.ErrUnauthorized
	before := sessions.calls
	if _, err := access.Evaluate(context.Background(), service.streamPolicy(record).Checker, request); err != nil {
		t.Fatal("trusted background execution required a browser session", err)
	}
	if sessions.calls != before {
		t.Fatal("background execution queried browser identity")
	}
	repository.valid = false
	if _, err := access.Evaluate(context.Background(), service.streamPolicy(record).Checker, request); err == nil {
		t.Fatal("background execution bypassed current Runner state")
	}
}
