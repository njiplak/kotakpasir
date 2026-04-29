// Package metrics holds the Prometheus instruments exported by kpd at /_metrics.
//
// The recorder is constructed once in main and threaded into the manager and
// reaper. Tests can pass NewNoop() to disable recording without nil checks.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Recorder is the surface manager + reaper use. The kpd Recorder is the
// concrete one; tests can use NewNoop().
type Recorder interface {
	SandboxCreated(image, profile, outcome string)
	SetSandboxActive(n int)
	PoolHit(image string)
	PoolMiss(image string)
	ReaperSwept(n int)
}

// Kpd is the live recorder backed by a private Prometheus registry. The
// registry is private so kpd doesn't expose go runtime / process metrics
// unless explicitly requested — keeps the surface area we promise stable.
type Kpd struct {
	registry *prometheus.Registry

	created   *prometheus.CounterVec
	active    prometheus.Gauge
	poolHit   *prometheus.CounterVec
	poolMiss  *prometheus.CounterVec
	reaperRun prometheus.Counter
}

func New() *Kpd {
	r := prometheus.NewRegistry()
	k := &Kpd{
		registry: r,
		created: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kpd_sandbox_created_total",
			Help: "Sandbox create attempts, labeled by image, profile, and outcome (success|policy_denied|runtime_error).",
		}, []string{"image", "profile", "outcome"}),
		active: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kpd_sandbox_active",
			Help: "Number of sandboxes in state=running. Updated on every Create/Delete.",
		}),
		poolHit: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kpd_pool_hit_total",
			Help: "Pool claims served from a warm container, labeled by image.",
		}, []string{"image"}),
		poolMiss: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kpd_pool_miss_total",
			Help: "Create requests that cold-started instead of using the pool, labeled by image.",
		}, []string{"image"}),
		reaperRun: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kpd_reaper_swept_total",
			Help: "Total expired sandboxes deleted by the reaper.",
		}),
	}
	r.MustRegister(k.created, k.active, k.poolHit, k.poolMiss, k.reaperRun)
	return k
}

func (k *Kpd) SandboxCreated(image, profile, outcome string) {
	k.created.WithLabelValues(image, profile, outcome).Inc()
}
func (k *Kpd) SetSandboxActive(n int)       { k.active.Set(float64(n)) }
func (k *Kpd) PoolHit(image string)         { k.poolHit.WithLabelValues(image).Inc() }
func (k *Kpd) PoolMiss(image string)        { k.poolMiss.WithLabelValues(image).Inc() }
func (k *Kpd) ReaperSwept(n int)            { k.reaperRun.Add(float64(n)) }

// Handler returns an http.Handler that serves the metrics in Prometheus exposition format.
func (k *Kpd) Handler() http.Handler {
	return promhttp.HandlerFor(k.registry, promhttp.HandlerOpts{})
}

// Noop is a Recorder that drops every call. Useful for tests.
type Noop struct{}

func NewNoop() *Noop                                          { return &Noop{} }
func (Noop) SandboxCreated(_, _, _ string)                    {}
func (Noop) SetSandboxActive(_ int)                           {}
func (Noop) PoolHit(_ string)                                 {}
func (Noop) PoolMiss(_ string)                                {}
func (Noop) ReaperSwept(_ int)                                {}
