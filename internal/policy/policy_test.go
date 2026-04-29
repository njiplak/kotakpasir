package policy_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"nexteam.id/kotakpasir/internal/policy"
)

func samplePolicy() *policy.Policy {
	allowlistMode := policy.EgressAllowlist
	imgEgress := &policy.Egress{Mode: allowlistMode, Hosts: []string{"api.openai.com", "169.254.169.254"}}
	profEgress := &policy.Egress{Mode: allowlistMode, Hosts: []string{"api.anthropic.com"}}

	return &policy.Policy{
		Version: 1,
		Defaults: policy.Defaults{
			Cpus: 1.0, MemoryMB: 512, PidsLimit: 256,
			User: "1000:1000", ReadOnly: true, NetworkMode: "none",
		},
		Images: []policy.Image{
			{Name: "alpine:latest"},
			{Name: "python:3.12-slim", Cpus: 2.0, MemoryMB: 1024, Egress: imgEgress},
		},
		Profiles: map[string]policy.Profile{
			"py-data": {Image: "python:3.12-slim", Cpus: 4.0, TTL: 10 * time.Minute, Egress: profEgress},
		},
		Egress: policy.GlobalEgress{
			Default:    policy.Egress{Mode: policy.EgressNone},
			GlobalDeny: []string{"169.254.169.254"},
		},
	}
}

func TestResolve_DefaultsOnly(t *testing.T) {
	p := samplePolicy()
	r, err := p.Resolve(policy.Request{Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Cpus != 1.0 || r.MemoryMB != 512 {
		t.Errorf("defaults not applied: cpus=%v mem=%v", r.Cpus, r.MemoryMB)
	}
	if r.Egress.Mode != policy.EgressNone {
		t.Errorf("egress=%q, want none (global default)", r.Egress.Mode)
	}
}

func TestResolve_ImageRulesOverrideDefaults(t *testing.T) {
	p := samplePolicy()
	r, err := p.Resolve(policy.Request{Image: "python:3.12-slim"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Cpus != 2.0 || r.MemoryMB != 1024 {
		t.Errorf("image rules not applied: cpus=%v mem=%v", r.Cpus, r.MemoryMB)
	}
	if r.Egress.Mode != policy.EgressAllowlist {
		t.Errorf("egress.mode=%q, want allowlist", r.Egress.Mode)
	}
	// global_deny strips 169.254.169.254 from the resolved hosts
	for _, h := range r.Egress.Hosts {
		if h == "169.254.169.254" {
			t.Errorf("global_deny did not filter 169.254.169.254 from hosts: %v", r.Egress.Hosts)
		}
	}
	if !contains(r.Egress.Hosts, "api.openai.com") {
		t.Errorf("expected api.openai.com in hosts, got %v", r.Egress.Hosts)
	}
}

func TestResolve_ProfileWithImageOverride(t *testing.T) {
	p := samplePolicy()
	r, err := p.Resolve(policy.Request{Profile: "py-data"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Image != "python:3.12-slim" {
		t.Errorf("Image=%q, want python:3.12-slim", r.Image)
	}
	// profile sets cpus=4.0; image entry sets 2.0; image wins (more specific)
	if r.Cpus != 2.0 {
		t.Errorf("Cpus=%v, want 2.0 (image overrides profile)", r.Cpus)
	}
	// profile's TTL should still apply (image entry has none)
	if r.TTL != 10*time.Minute {
		t.Errorf("TTL=%v, want 10m", r.TTL)
	}
	// image entry's egress overrides profile's
	if !contains(r.Egress.Hosts, "api.openai.com") {
		t.Errorf("image entry egress should win: hosts=%v", r.Egress.Hosts)
	}
}

func TestResolve_RequestOverridesAll(t *testing.T) {
	p := samplePolicy()
	r, err := p.Resolve(policy.Request{
		Profile:  "py-data",
		Cpus:     8.0,
		MemoryMB: 4096,
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Cpus != 8.0 || r.MemoryMB != 4096 || r.TTL != time.Hour {
		t.Errorf("request overrides not applied: cpus=%v mem=%v ttl=%v", r.Cpus, r.MemoryMB, r.TTL)
	}
}

func TestResolve_DeniesUnlistedImage(t *testing.T) {
	p := samplePolicy()
	_, err := p.Resolve(policy.Request{Image: "ubuntu"})
	if err == nil || !strings.Contains(err.Error(), "not in policy.images allowlist") {
		t.Fatalf("want allowlist denial, got: %v", err)
	}
}

func TestResolve_RequiresImageOrProfile(t *testing.T) {
	p := samplePolicy()
	_, err := p.Resolve(policy.Request{})
	if err == nil || !strings.Contains(err.Error(), "image is required") {
		t.Fatalf("want image-required error, got: %v", err)
	}
}

func TestResolve_MissingProfile(t *testing.T) {
	p := samplePolicy()
	_, err := p.Resolve(policy.Request{Profile: "ghost"})
	if err == nil || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("want profile-not-found error, got: %v", err)
	}
}

func TestDefault_PermissiveAllowsAnyImage(t *testing.T) {
	d := policy.Default()
	if _, err := d.Resolve(policy.Request{Image: "anything:tag"}); err != nil {
		t.Errorf("default policy rejected image: %v", err)
	}
}

func TestValidate_VersionMismatch(t *testing.T) {
	p := &policy.Policy{Version: 2}
	if err := p.Validate(); err == nil {
		t.Error("Validate accepted version=2")
	}
}

func TestValidate_BadEgressMode(t *testing.T) {
	p := &policy.Policy{Version: 1, Egress: policy.GlobalEgress{Default: policy.Egress{Mode: "bogus"}}}
	if err := p.Validate(); err == nil {
		t.Error("Validate accepted bogus egress mode")
	}
}

func contains(xs []string, x string) bool {
	return slices.Contains(xs, x)
}
