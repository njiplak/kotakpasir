// Package policy is the source of truth for what a sandbox is allowed to be.
// It loads from YAML and exposes Resolve(), which merges defaults, profile,
// per-image rules, and per-request fields into a single effective spec.
package policy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

const CurrentVersion = 1

// ErrPolicyViolation is the sentinel wrapped by every Resolve() error.
// Callers (kpd HTTP handlers) use errors.Is to map these to 400 responses
// rather than 500 — they're user-facing input errors, not server faults.
var ErrPolicyViolation = errors.New("policy violation")

type Policy struct {
	Version  int                `yaml:"version"`
	Defaults Defaults           `yaml:"defaults"`
	Images   []Image            `yaml:"images"`
	Profiles map[string]Profile `yaml:"profiles"`
	Egress   GlobalEgress       `yaml:"egress"`
}

type Defaults struct {
	Cpus        float64       `yaml:"cpus"`
	MemoryMB    int64         `yaml:"memory_mb"`
	PidsLimit   int64         `yaml:"pids_limit"`
	User        string        `yaml:"user"`
	ReadOnly    bool          `yaml:"read_only"`
	NetworkMode string        `yaml:"network_mode"`
	TTL         time.Duration `yaml:"ttl"`
	Runtime     string        `yaml:"runtime"`
}

type Image struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description,omitempty"`
	Cpus        float64       `yaml:"cpus,omitempty"`
	MemoryMB    int64         `yaml:"memory_mb,omitempty"`
	TTL         time.Duration `yaml:"ttl,omitempty"`
	Egress      *Egress       `yaml:"egress,omitempty"`
	// Pool is the number of warm pre-started containers to keep ready for
	// this image. 0 disables pooling (cold start every time). The pool is
	// only used when a request resolves to defaults (no per-request cpu/memory
	// overrides) and egress.mode is "none" or empty.
	Pool int `yaml:"pool,omitempty"`
}

type Profile struct {
	Image    string        `yaml:"image"`
	Cpus     float64       `yaml:"cpus,omitempty"`
	MemoryMB int64         `yaml:"memory_mb,omitempty"`
	TTL      time.Duration `yaml:"ttl,omitempty"`
	Egress   *Egress       `yaml:"egress,omitempty"`
}

type GlobalEgress struct {
	Default    Egress   `yaml:"default"`
	GlobalDeny []string `yaml:"global_deny,omitempty"`
}

type Egress struct {
	Mode  string   `yaml:"mode"`
	Hosts []string `yaml:"hosts,omitempty"`
}

const (
	EgressNone      = "none"
	EgressAllowlist = "allowlist"
)

// Default returns a permissive policy used when no YAML file is present.
// It mirrors the historical zero-config behavior: hardened defaults, no
// allowlist, no profiles, egress disabled.
func Default() *Policy {
	return &Policy{
		Version: CurrentVersion,
		Defaults: Defaults{
			Cpus:        1.0,
			MemoryMB:    512,
			PidsLimit:   256,
			User:        "1000:1000",
			ReadOnly:    true,
			NetworkMode: "none",
			TTL:         0,
		},
		Egress: GlobalEgress{Default: Egress{Mode: EgressNone}},
	}
}

// Load reads, parses, and validates a policy file. If path is empty or the
// file does not exist, Default() is returned (permissive mode).
func Load(path string) (*Policy, error) {
	if path == "" {
		return Default(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Default(), nil
		}
		return nil, fmt.Errorf("read policy %q: %w", path, err)
	}
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse policy %q: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid policy %q: %w", path, err)
	}
	return &p, nil
}

func (p *Policy) Validate() error {
	if p.Version != CurrentVersion {
		return fmt.Errorf("unsupported version %d (want %d)", p.Version, CurrentVersion)
	}

	imageNames := make(map[string]struct{}, len(p.Images))
	for i, img := range p.Images {
		if img.Name == "" {
			return fmt.Errorf("images[%d]: name is required", i)
		}
		if _, dup := imageNames[img.Name]; dup {
			return fmt.Errorf("images[%d]: duplicate image %q", i, img.Name)
		}
		imageNames[img.Name] = struct{}{}
		if img.Egress != nil {
			if err := img.Egress.Validate(); err != nil {
				return fmt.Errorf("images[%s].egress: %w", img.Name, err)
			}
		}
		if img.Pool < 0 {
			return fmt.Errorf("images[%s]: pool must be >= 0", img.Name)
		}
		if img.Pool > 0 && img.Egress != nil && img.Egress.Mode != "" && img.Egress.Mode != EgressNone {
			return fmt.Errorf("images[%s]: pool not supported with egress.mode=%q (per-sandbox proxy is incompatible with pre-warming)", img.Name, img.Egress.Mode)
		}
	}

	for name, prof := range p.Profiles {
		if prof.Image == "" {
			return fmt.Errorf("profiles[%s]: image is required", name)
		}
		if len(p.Images) > 0 {
			if _, ok := imageNames[prof.Image]; !ok {
				return fmt.Errorf("profiles[%s]: image %q not in images list", name, prof.Image)
			}
		}
		if prof.Egress != nil {
			if err := prof.Egress.Validate(); err != nil {
				return fmt.Errorf("profiles[%s].egress: %w", name, err)
			}
		}
	}

	if err := p.Egress.Default.Validate(); err != nil {
		return fmt.Errorf("egress.default: %w", err)
	}

	if p.Defaults.NetworkMode != "" && p.Defaults.NetworkMode != "none" && p.Defaults.NetworkMode != "bridge" {
		return fmt.Errorf("defaults.network_mode=%q invalid (want none|bridge)", p.Defaults.NetworkMode)
	}
	return nil
}

func (e *Egress) Validate() error {
	switch e.Mode {
	case "", EgressNone, EgressAllowlist:
		return nil
	default:
		return fmt.Errorf("mode=%q invalid (want none|allowlist)", e.Mode)
	}
}

// Request is the user-supplied input to Resolve. Empty fields fall through
// to profile, image, and defaults in that order.
type Request struct {
	Profile  string
	Image    string
	Cmd      []string
	Env      map[string]string
	Cpus     float64
	MemoryMB int64
	TTL      time.Duration
	Name     string
	Labels   map[string]string
}

// Resolved is the effective spec after merging defaults, profile, image, and request.
// global_deny is applied to Egress.Hosts at the very end.
type Resolved struct {
	Image       string
	Cmd         []string
	Env         map[string]string
	Cpus        float64
	MemoryMB    int64
	PidsLimit   int64
	User        string
	ReadOnly    bool
	NetworkMode string
	TTL         time.Duration
	RuntimeName string
	Egress      Egress
	Name        string
	Labels      map[string]string
}

// Resolve computes the effective spec. Precedence (most → least specific):
//   request → image entry → profile → defaults
//
// Returns an error if the resolved image is non-empty but not in the allowlist
// (when an allowlist is in effect — i.e., len(Images) > 0).
func (p *Policy) Resolve(req Request) (Resolved, error) {
	out := Resolved{
		Cpus:        p.Defaults.Cpus,
		MemoryMB:    p.Defaults.MemoryMB,
		PidsLimit:   p.Defaults.PidsLimit,
		User:        p.Defaults.User,
		ReadOnly:    p.Defaults.ReadOnly,
		NetworkMode: p.Defaults.NetworkMode,
		TTL:         p.Defaults.TTL,
		RuntimeName: p.Defaults.Runtime,
		Egress:      p.Egress.Default,
	}

	if req.Profile != "" {
		prof, ok := p.Profiles[req.Profile]
		if !ok {
			return out, fmt.Errorf("%w: profile %q not found", ErrPolicyViolation, req.Profile)
		}
		out.Image = prof.Image
		if prof.Cpus > 0 {
			out.Cpus = prof.Cpus
		}
		if prof.MemoryMB > 0 {
			out.MemoryMB = prof.MemoryMB
		}
		if prof.TTL > 0 {
			out.TTL = prof.TTL
		}
		if prof.Egress != nil {
			out.Egress = *prof.Egress
		}
	}

	if req.Image != "" {
		out.Image = req.Image
	}
	if out.Image == "" {
		return out, fmt.Errorf("%w: image is required (set request.Image or use a profile)", ErrPolicyViolation)
	}

	if img, ok := p.findImage(out.Image); ok {
		if img.Cpus > 0 {
			out.Cpus = img.Cpus
		}
		if img.MemoryMB > 0 {
			out.MemoryMB = img.MemoryMB
		}
		if img.TTL > 0 {
			out.TTL = img.TTL
		}
		if img.Egress != nil {
			out.Egress = *img.Egress
		}
	} else if len(p.Images) > 0 {
		return out, fmt.Errorf("%w: image %q not in policy.images allowlist", ErrPolicyViolation, out.Image)
	}

	if req.Cpus > 0 {
		out.Cpus = req.Cpus
	}
	if req.MemoryMB > 0 {
		out.MemoryMB = req.MemoryMB
	}
	if req.TTL > 0 {
		out.TTL = req.TTL
	}
	out.Cmd = req.Cmd
	out.Env = req.Env
	out.Name = req.Name
	out.Labels = req.Labels

	out.Egress.Hosts = applyGlobalDeny(out.Egress.Hosts, p.Egress.GlobalDeny)
	return out, nil
}

func (p *Policy) findImage(name string) (Image, bool) {
	for _, img := range p.Images {
		if img.Name == name {
			return img, true
		}
	}
	return Image{}, false
}

func applyGlobalDeny(hosts, deny []string) []string {
	if len(hosts) == 0 || len(deny) == 0 {
		return hosts
	}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if !slices.Contains(deny, h) {
			out = append(out, h)
		}
	}
	return out
}
