package docker_test

import (
	"os"
	"testing"

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
