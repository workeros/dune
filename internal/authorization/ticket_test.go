package authorization

import "testing"

func TestConnectionScopeRetainsPrincipalKind(t *testing.T) {
	record := ConnectionAccess{
		PrincipalID: "service_account:runner-service", PrincipalKind: "service_account",
		Namespace: "sanddance", Subject: "runner-service", OwnerID: "tenant-1",
		Target: "machine-1", RunnerID: "runner-1", FabricID: "tae-sandbox",
		BindingRevision: 3,
	}
	scope := record.Scope()
	if scope.PrincipalKind != record.PrincipalKind || scope.PrincipalID != record.PrincipalID || scope.Subject != record.Subject || scope.OwnerID != record.OwnerID {
		t.Fatalf("connection scope lost enterprise principal identity: %+v", scope)
	}
}
