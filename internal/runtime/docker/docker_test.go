package docker_test

import (
	"context"
	"os"
	"testing"

	dockerapi "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"

	"nexteam.id/kotakpasir/internal/pool"
	rt "nexteam.id/kotakpasir/internal/runtime"
	"nexteam.id/kotakpasir/internal/runtime/docker"
	"nexteam.id/kotakpasir/internal/runtime/runtimetest"
)

// TestDockerConformance runs the Runtime contract suite against a real Docker
// daemon. Opt-in via KP_DOCKER_TESTS=1 since CI without Docker would otherwise fail.
//
// Prerequisites:
//   - Docker daemon reachable
//   - "alpine:latest" image pulled locally (or network access for pull)
func TestDockerConformance(t *testing.T) {
	if os.Getenv("KP_DOCKER_TESTS") != "1" {
		t.Skip("set KP_DOCKER_TESTS=1 to run integration tests against Docker daemon")
	}
	runtimetest.Suite(t, func(t *testing.T) rt.Runtime {
		r, err := docker.New()
		if err != nil {
			t.Fatalf("docker.New: %v", err)
		}
		return r
	})
}

// TestDockerCleanPoolOrphans plants a container with the warm-pool labels
// kpd uses to identify its own work, then verifies CleanPoolOrphans removes
// it. This guards the recovery path that runs at kpd startup so a previous
// crash doesn't leak warm containers across restarts.
func TestDockerCleanPoolOrphans(t *testing.T) {
	if os.Getenv("KP_DOCKER_TESTS") != "1" {
		t.Skip("set KP_DOCKER_TESTS=1 to run integration tests against Docker daemon")
	}

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	ctx := t.Context()

	const orphanImage = "alpine:latest"
	cfg := &dockerapi.Config{
		Image: orphanImage,
		Cmd:   []string{"tail", "-f", "/dev/null"},
		Labels: map[string]string{
			pool.LabelRole:        pool.RolePoolWarm,
			pool.LabelPoolImage:   orphanImage,
			"kotakpasir.managed": "true",
		},
	}
	created, err := cli.ContainerCreate(ctx, cfg, &dockerapi.HostConfig{}, nil, nil, "")
	if err != nil {
		t.Fatalf("plant orphan: %v", err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), created.ID, dockerapi.RemoveOptions{Force: true})
	})

	if err := cli.ContainerStart(ctx, created.ID, dockerapi.StartOptions{}); err != nil {
		t.Fatalf("start orphan: %v", err)
	}

	r, err := docker.New()
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	defer r.Close()

	if err := r.CleanPoolOrphans(ctx); err != nil {
		t.Fatalf("CleanPoolOrphans: %v", err)
	}

	// Verify nothing carrying the warm-pool label is left.
	f := filters.NewArgs()
	f.Add("label", pool.LabelRole+"="+pool.RolePoolWarm)
	containers, err := cli.ContainerList(ctx, dockerapi.ListOptions{All: true, Filters: f})
	if err != nil {
		t.Fatalf("list after clean: %v", err)
	}
	if len(containers) != 0 {
		t.Errorf("after CleanPoolOrphans: %d warm containers remain, want 0", len(containers))
	}
}

