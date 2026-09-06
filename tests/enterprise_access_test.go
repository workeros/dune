package tests

import (
	"context"
	"github.com/aiomni/dune/pkg/access"
	"testing"
)

type enterpriseCheck func(context.Context, access.Request) (access.Decision, error)

func (f enterpriseCheck) Check(ctx context.Context, r access.Request) (access.Decision, error) {
	return f(ctx, r)
}
func TestEnterpriseSharedExecutionAndRevocation(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testPrefixedWorkbench(t, workbenchCase{enterprise: true, humanCLI: true}) })
	t.Run("postgres-separate-gateway", func(t *testing.T) {
		database := postgresWorkbenchConfig(t)
		testPrefixedWorkbench(t, workbenchCase{database: &database, separateGateway: true, enterprise: true, humanCLI: true})
	})
}
