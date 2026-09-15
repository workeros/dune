package fabric

import (
	"strings"
	"testing"
)

func bootstrapCall() BootstrapCall {
	return BootstrapCall{
		Action:          Action{ID: "bootstrap-action-1", ResourceRef: "sandbox-resource-1"},
		EnrollmentToken: strings.Repeat("a", 64),
		Endpoint:        "https://DUNE.example.test/base",
		GatewayURL:      "wss://gateway.example.test/base/api/v1/ws/tunnel",
		Version:         "fabricd-v2",
	}
}

func TestBootstrapPlanSelectsPlatformAndKeepsProviderTransportOut(t *testing.T) {
	call := bootstrapCall()
	plan, err := NewBootstrapPlan(call, BootstrapPlatform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Root == "" || plan.CompletionPath != plan.Root+"/bootstrap.complete" || plan.Platform != (BootstrapPlatform{OS: "linux", Arch: "arm64"}) {
		t.Fatalf("incomplete bootstrap plan: %+v", plan)
	}
	for _, expected := range []string{
		"https://dune.example.test/base/api/v1/downloads/dune-linux-arm64.tar.gz",
		" enroll --site 'https://dune.example.test/base/' --token '",
		" fabricd >\"$root/fabricd.log\"",
		"bootstrap.complete.tmp",
		"'bootstrap-action-1' 'fabricd-v2'",
	} {
		if !strings.Contains(plan.Script, expected) {
			t.Fatalf("bootstrap plan missing %q:\n%s", expected, plan.Script)
		}
	}
	for _, providerDetail := range []string{"/api/process/start", "/api/fs/stat", "tae"} {
		if strings.Contains(strings.ToLower(plan.Script), providerDetail) {
			t.Fatalf("provider transport leaked into bootstrap plan: %q", providerDetail)
		}
	}
	marker, err := BootstrapCompletionPath(call.Action.ID)
	if err != nil || marker != plan.CompletionPath {
		t.Fatal("reconciliation marker changed", marker, err)
	}
}

func TestBootstrapPlanRejectsInvalidOrAmbiguousInputs(t *testing.T) {
	valid := bootstrapCall()
	tests := []struct {
		name     string
		change   func(*BootstrapCall)
		platform BootstrapPlatform
	}{
		{name: "platform", platform: BootstrapPlatform{OS: "linux", Arch: "386"}},
		{name: "action", platform: BootstrapPlatform{OS: "linux", Arch: "amd64"}, change: func(c *BootstrapCall) { c.Action.ID = "" }},
		{name: "resource", platform: BootstrapPlatform{OS: "linux", Arch: "amd64"}, change: func(c *BootstrapCall) { c.Action.ResourceRef = "" }},
		{name: "token length", platform: BootstrapPlatform{OS: "linux", Arch: "amd64"}, change: func(c *BootstrapCall) { c.EnrollmentToken = "short" }},
		{name: "token alphabet", platform: BootstrapPlatform{OS: "linux", Arch: "amd64"}, change: func(c *BootstrapCall) { c.EnrollmentToken = strings.Repeat("z", 64) }},
		{name: "version", platform: BootstrapPlatform{OS: "linux", Arch: "amd64"}, change: func(c *BootstrapCall) { c.Version = "" }},
		{name: "endpoint", platform: BootstrapPlatform{OS: "linux", Arch: "amd64"}, change: func(c *BootstrapCall) { c.Endpoint = "file:///tmp/dune" }},
		{name: "downgrade", platform: BootstrapPlatform{OS: "linux", Arch: "amd64"}, change: func(c *BootstrapCall) { c.GatewayURL = "ws://gateway.example.test/base/api/v1/ws/tunnel" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			call := valid
			if test.change != nil {
				test.change(&call)
			}
			if _, err := NewBootstrapPlan(call, test.platform); err == nil {
				t.Fatal("invalid bootstrap plan accepted")
			}
		})
	}
}
