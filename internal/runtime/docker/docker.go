package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	rt "nexteam.id/kotakpasir/internal/runtime"
)

const (
	containerNamePrefix = "kp-"
	proxyNamePrefix     = "kp-proxy-"
	networkNamePrefix   = "kp-net-"

	labelSandboxID = "kotakpasir.sandbox-id"
	labelRole      = "kotakpasir.role"
	labelManaged   = "kotakpasir.managed"

	roleProxy   = "proxy"
	roleNetwork = "network"

	defaultStopTimeout = 5
	defaultProxyImage  = "kotakpasir/proxy:dev"
)

var (
	defaultKeepAlive = []string{"tail", "-f", "/dev/null"}

	// Always denied even if listed in any allowlist. Defense in depth.
	defaultDenyHosts = []string{
		"169.254.169.254",
		"metadata.google.internal",
		"metadata.aws.internal",
	}
)

type Runtime struct {
	cli *client.Client
}

func New() (*Runtime, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Runtime{cli: cli}, nil
}

func (r *Runtime) Close() error {
	return r.cli.Close()
}

// Ping verifies the Docker daemon is reachable. Cheap; safe to call from /healthz.
func (r *Runtime) Ping(ctx context.Context) error {
	_, err := r.cli.Ping(ctx)
	return err
}

func (r *Runtime) Create(ctx context.Context, spec rt.Spec) (rt.Handle, error) {
	sandboxID := ""
	if spec.Labels != nil {
		sandboxID = spec.Labels[labelSandboxID]
	}

	networkName, proxyID, err := r.maybeProvisionEgress(ctx, sandboxID, spec)
	if err != nil {
		return rt.Handle{}, err
	}

	rollback := func() {
		if proxyID != "" {
			_ = r.cli.ContainerRemove(ctx, proxyID, container.RemoveOptions{Force: true})
		}
		if networkName != "" {
			_ = r.cli.NetworkRemove(ctx, networkName)
		}
	}

	cfg, hostCfg, netCfg := buildContainerConfig(spec, networkName)

	name := ""
	if spec.Name != "" {
		name = containerNamePrefix + spec.Name
	}

	resp, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, name)
	if errdefs.IsNotFound(err) {
		if perr := r.pullImage(ctx, spec.Image); perr != nil {
			rollback()
			return rt.Handle{}, fmt.Errorf("pull image %q: %w", spec.Image, perr)
		}
		resp, err = r.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, name)
	}
	if err != nil {
		rollback()
		return rt.Handle{}, fmt.Errorf("container create: %w", err)
	}

	if err := r.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = r.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		rollback()
		return rt.Handle{}, fmt.Errorf("container start: %w", err)
	}

	return rt.Handle{ID: resp.ID, Name: name}, nil
}

// maybeProvisionEgress creates a per-sandbox network and starts the proxy
// container if egress.mode is "allowlist". Returns (networkName, proxyContainerID).
// On any error inside, it rolls back partially-created resources before returning.
func (r *Runtime) maybeProvisionEgress(ctx context.Context, sandboxID string, spec rt.Spec) (string, string, error) {
	switch spec.Egress.Mode {
	case "", "none":
		return "", "", nil
	case "allowlist":
		// proceed
	default:
		return "", "", fmt.Errorf("egress mode %q invalid", spec.Egress.Mode)
	}

	if sandboxID == "" {
		return "", "", errors.New("egress allowlist requires kotakpasir.sandbox-id label on spec")
	}

	networkName := networkNamePrefix + sandboxID
	commonLabels := map[string]string{
		labelSandboxID: sandboxID,
		labelManaged:   "true",
	}
	netLabels := map[string]string{
		labelSandboxID: sandboxID,
		labelRole:      roleNetwork,
		labelManaged:   "true",
	}

	// Internal=true means the network has NO route to the host/internet.
	// Sandbox + proxy share this network (sandbox can only reach proxy).
	// The proxy gets connected to "bridge" separately for actual internet egress.
	if _, err := r.cli.NetworkCreate(ctx, networkName, network.CreateOptions{
		Driver:   "bridge",
		Internal: true,
		Labels:   netLabels,
	}); err != nil {
		return "", "", fmt.Errorf("network create: %w", err)
	}

	proxyImage := os.Getenv("KP_PROXY_IMAGE")
	if proxyImage == "" {
		proxyImage = defaultProxyImage
	}

	proxyEnv := []string{
		"KP_ALLOWED_HOSTS=" + strings.Join(spec.Egress.Hosts, ","),
		"KP_DENY_HOSTS=" + strings.Join(defaultDenyHosts, ","),
		"KP_PROXY_PORT=8080",
	}

	proxyCfg := &container.Config{
		Image: proxyImage,
		Labels: mergeMap(commonLabels, map[string]string{labelRole: roleProxy}),
		Env:   proxyEnv,
	}
	pidsLimit := int64(64)
	memBytes := int64(64 * 1024 * 1024)
	proxyHost := &container.HostConfig{
		NetworkMode:    container.NetworkMode(networkName),
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		// Auto-restart on crash, OOM, or daemon reboot. Force-removal during
		// teardown bypasses this (the container is gone, not just stopped).
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		Resources: container.Resources{
			Memory:    memBytes,
			PidsLimit: &pidsLimit,
		},
	}
	proxyNet := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			networkName: {Aliases: []string{"proxy"}},
		},
	}

	proxyResp, err := r.cli.ContainerCreate(ctx, proxyCfg, proxyHost, proxyNet, nil, proxyNamePrefix+sandboxID)
	if errdefs.IsNotFound(err) {
		_ = r.cli.NetworkRemove(ctx, networkName)
		return "", "", fmt.Errorf("proxy image %q not found — build with: docker build -f Dockerfile.kpproxy -t %s .", proxyImage, proxyImage)
	}
	if err != nil {
		_ = r.cli.NetworkRemove(ctx, networkName)
		return "", "", fmt.Errorf("proxy create: %w", err)
	}

	if err := r.cli.ContainerStart(ctx, proxyResp.ID, container.StartOptions{}); err != nil {
		_ = r.cli.ContainerRemove(ctx, proxyResp.ID, container.RemoveOptions{Force: true})
		_ = r.cli.NetworkRemove(ctx, networkName)
		return "", "", fmt.Errorf("proxy start: %w", err)
	}

	// Connect the proxy to the default bridge so it can reach the internet.
	// Sandbox is NOT on this network, so direct egress from the sandbox is impossible.
	if err := r.cli.NetworkConnect(ctx, "bridge", proxyResp.ID, nil); err != nil {
		_ = r.cli.ContainerRemove(ctx, proxyResp.ID, container.RemoveOptions{Force: true})
		_ = r.cli.NetworkRemove(ctx, networkName)
		return "", "", fmt.Errorf("proxy attach to bridge: %w", err)
	}

	slog.Debug("egress proxy provisioned", "sandbox", sandboxID, "network", networkName, "proxy", proxyResp.ID, "hosts", len(spec.Egress.Hosts))
	return networkName, proxyResp.ID, nil
}

func (r *Runtime) Exec(ctx context.Context, id string, spec rt.ExecSpec) (rt.ExecResult, error) {
	start := time.Now()

	attachStdin := spec.Stdin != nil

	createResp, err := r.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd:          spec.Cmd,
		Env:          flattenEnv(spec.Env),
		WorkingDir:   spec.WorkDir,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  attachStdin,
		Tty:          false,
	})
	if err != nil {
		return rt.ExecResult{}, fmt.Errorf("exec create: %w", err)
	}

	attach, err := r.cli.ContainerExecAttach(ctx, createResp.ID, container.ExecStartOptions{Tty: false})
	if err != nil {
		return rt.ExecResult{}, fmt.Errorf("exec attach: %w", err)
	}
	defer attach.Close()

	if attachStdin {
		go func() {
			_, _ = io.Copy(attach.Conn, spec.Stdin)
			_ = attach.CloseWrite()
		}()
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutW := io.Writer(&stdoutBuf)
	stderrW := io.Writer(&stderrBuf)
	if spec.Stdout != nil {
		stdoutW = spec.Stdout
	}
	if spec.Stderr != nil {
		stderrW = spec.Stderr
	}

	if _, err := stdcopy.StdCopy(stdoutW, stderrW, attach.Reader); err != nil && !errors.Is(err, io.EOF) {
		return rt.ExecResult{}, fmt.Errorf("exec read: %w", err)
	}

	insp, err := r.cli.ContainerExecInspect(ctx, createResp.ID)
	if err != nil {
		return rt.ExecResult{}, fmt.Errorf("exec inspect: %w", err)
	}

	res := rt.ExecResult{
		ExitCode: insp.ExitCode,
		Duration: time.Since(start),
	}
	if spec.Stdout == nil {
		res.Stdout = stdoutBuf.String()
	}
	if spec.Stderr == nil {
		res.Stderr = stderrBuf.String()
	}
	return res, nil
}

func (r *Runtime) Stop(ctx context.Context, id string) error {
	timeout := defaultStopTimeout
	return r.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
}

func (r *Runtime) Remove(ctx context.Context, id string) error {
	// Best-effort: read the sandbox grouping label off the container before
	// removing it, so we can clean up the proxy + network too.
	sandboxID := ""
	if insp, err := r.cli.ContainerInspect(ctx, id); err == nil && insp.Config != nil {
		sandboxID = insp.Config.Labels[labelSandboxID]
	}

	if err := r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !errdefs.IsNotFound(err) {
		slog.Warn("primary container remove failed", "id", id, "err", err)
	}

	if sandboxID == "" {
		return nil
	}

	r.cleanupSandboxGroup(ctx, sandboxID)
	return nil
}

// cleanupSandboxGroup removes any remaining containers and networks tagged
// with the given sandbox id. Used both during Remove and as a defensive
// cleanup path on failed Create.
func (r *Runtime) cleanupSandboxGroup(ctx context.Context, sandboxID string) {
	listFilter := filters.NewArgs()
	listFilter.Add("label", labelSandboxID+"="+sandboxID)

	related, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: listFilter})
	if err == nil {
		for _, c := range related {
			_ = r.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
		}
	}

	netFilter := filters.NewArgs()
	netFilter.Add("label", labelSandboxID+"="+sandboxID)

	nets, err := r.cli.NetworkList(ctx, network.ListOptions{Filters: netFilter})
	if err == nil {
		for _, n := range nets {
			_ = r.cli.NetworkRemove(ctx, n.ID)
		}
	}
}

// CleanPoolOrphans removes any leftover warm-pool containers from previous
// kpd runs. Pool entries are identified by the kotakpasir.role=pool-warm label.
// Best-effort: errors are logged but never returned to the caller, since failed
// orphan cleanup shouldn't block startup.
func (r *Runtime) CleanPoolOrphans(ctx context.Context) error {
	f := filters.NewArgs()
	f.Add("label", labelRole+"=pool-warm")

	containers, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return fmt.Errorf("list pool orphans: %w", err)
	}
	if len(containers) == 0 {
		return nil
	}

	for _, c := range containers {
		if err := r.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			slog.Warn("pool orphan remove", "id", c.ID, "err", err)
		}
	}
	slog.Info("pool orphans cleaned", "count", len(containers))
	return nil
}

func (r *Runtime) Status(ctx context.Context, id string) (rt.Status, error) {
	insp, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return rt.Status{}, err
	}
	st := rt.Status{State: insp.State.Status}
	if insp.State.ExitCode != 0 || insp.State.Status == "exited" {
		code := insp.State.ExitCode
		st.ExitCode = &code
	}
	return st, nil
}

// ProxyAddr returns "<ip>:8080" of the egress proxy associated with sandboxID,
// using the proxy's docker-bridge endpoint (routable from the host — useful for
// pointing Prometheus or curl at it). Returns rt.ErrNoProxy when no proxy
// container exists for the sandbox (e.g. egress=none).
func (r *Runtime) ProxyAddr(ctx context.Context, sandboxID string) (string, error) {
	insp, err := r.cli.ContainerInspect(ctx, proxyNamePrefix+sandboxID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return "", rt.ErrNoProxy
		}
		return "", fmt.Errorf("inspect proxy: %w", err)
	}
	if insp.NetworkSettings == nil {
		return "", fmt.Errorf("proxy %s has no network settings", sandboxID)
	}
	bridge, ok := insp.NetworkSettings.Networks["bridge"]
	if !ok || bridge.IPAddress == "" {
		return "", fmt.Errorf("proxy %s not attached to bridge yet", sandboxID)
	}
	return bridge.IPAddress + ":8080", nil
}

// EnsureImage makes sure ref is present locally, pulling it if not.
// Returns pulled=true when a pull was actually performed (vs. cache hit).
// Used by the manager at startup to surface long pulls as explicit log lines
// instead of silently blocking pool warm-up.
func (r *Runtime) EnsureImage(ctx context.Context, ref string) (pulled bool, err error) {
	if _, ierr := r.cli.ImageInspect(ctx, ref); ierr == nil {
		return false, nil
	} else if !errdefs.IsNotFound(ierr) {
		return false, fmt.Errorf("inspect image %q: %w", ref, ierr)
	}
	if perr := r.pullImage(ctx, ref); perr != nil {
		return false, fmt.Errorf("pull image %q: %w", ref, perr)
	}
	return true, nil
}

func (r *Runtime) pullImage(ctx context.Context, ref string) error {
	rc, err := r.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

func buildContainerConfig(spec rt.Spec, networkName string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	cmd := spec.Cmd
	if len(cmd) == 0 {
		cmd = defaultKeepAlive
	}

	env := flattenEnv(spec.Env)

	netMode := networkOrDefault(spec.NetworkMode)
	var netCfg *network.NetworkingConfig

	if networkName != "" {
		// Egress proxy provisioned: place sandbox on the per-sandbox network
		// and inject HTTPS_PROXY env so the agent's HTTP client uses it.
		netMode = networkName
		env = append(env,
			"HTTPS_PROXY=http://proxy:8080",
			"HTTP_PROXY=http://proxy:8080",
			"NO_PROXY=",
		)
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				networkName: {},
			},
		}
	}

	cfg := &container.Config{
		Image:        spec.Image,
		Cmd:          cmd,
		User:         spec.User,
		Env:          env,
		Labels:       spec.Labels,
		Tty:          false,
		AttachStdin:  false,
		AttachStdout: false,
		AttachStderr: false,
		OpenStdin:    false,
	}

	pidsLimit := spec.PidsLimit

	hostCfg := &container.HostConfig{
		ReadonlyRootfs: spec.ReadOnly,
		NetworkMode:    container.NetworkMode(netMode),
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		Runtime:        spec.RuntimeName,
		Tmpfs: map[string]string{
			"/tmp": "rw,size=64m,mode=1777",
		},
		Resources: container.Resources{
			NanoCPUs:  int64(spec.Cpus * 1e9),
			Memory:    spec.MemoryMB * 1024 * 1024,
			PidsLimit: &pidsLimit,
		},
	}
	return cfg, hostCfg, netCfg
}

func networkOrDefault(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func flattenEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func mergeMap[K comparable, V any](base, extra map[K]V) map[K]V {
	out := make(map[K]V, len(base)+len(extra))
	maps.Copy(out, base)
	maps.Copy(out, extra)
	return out
}

var _ rt.Runtime = (*Runtime)(nil)
