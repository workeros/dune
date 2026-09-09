package tests

import (
	"context"
	"testing"

	"github.com/aiomni/dune/pkg/access"
)

type enterpriseCheck func(context.Context, access.Request) (access.Decision, error)

func (f enterpriseCheck) Check(ctx context.Context, r access.Request) (access.Decision, error) {
	return f(ctx, r)
}
func TestEnterpriseSharedExecutionAndRevocation(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testPrefixedWorkbench(t, workbenchCase{enterprise: true, runnerEntry: true})
	})
	t.Run("postgres-separate-gateway", func(t *testing.T) {
		database := postgresWorkbenchConfig(t)
		testPrefixedWorkbench(t, workbenchCase{database: &database, separateGateway: true, enterprise: true, runnerEntry: true})
	})
}
