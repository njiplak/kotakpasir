// Package proxy implements the per-sandbox HTTPS CONNECT proxy that enforces
// the egress allowlist. It deliberately does not MITM TLS — see docs/security-model.md.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/elazarl/goproxy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	healthPath  = "/_health"
	metricsPath = "/_metrics"
)

type Config struct {
	ListenAddr   string
	AllowedHosts []string
	DenyHosts    []string
}

type Server struct {
	cfg     Config
	srv     *http.Server
	metrics *metrics
}

type metrics struct {
	registry *prometheus.Registry
	allow    *prometheus.CounterVec
	deny     *prometheus.CounterVec
}

func newMetrics() *metrics {
	r := prometheus.NewRegistry()
	m := &metrics{
		registry: r,
		allow: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kpproxy_connect_allow_total",
			Help: "Number of CONNECT requests permitted by the allowlist, labeled by target host.",
		}, []string{"host"}),
		deny: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kpproxy_connect_deny_total",
			Help: "Number of CONNECT requests denied, labeled by target host and reason (global_deny|not_in_allowlist|parse_error).",
		}, []string{"host", "reason"}),
	}
	r.MustRegister(m.allow, m.deny)
	return m
}

func New(cfg Config) *Server {
	m := newMetrics()

	gp := goproxy.NewProxyHttpServer()
	gp.Verbose = false

	allowed := normalize(cfg.AllowedHosts)
	denied := normalize(cfg.DenyHosts)

	gp.OnRequest().HandleConnectFunc(func(host string, _ *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		hostname, _, err := splitHostPort(host)
		if err != nil {
			m.deny.WithLabelValues(host, "parse_error").Inc()
			slog.Warn("proxy connect parse failed", "host", host, "err", err)
			return goproxy.RejectConnect, host
		}
		if isDenied(hostname, denied) {
			m.deny.WithLabelValues(hostname, "global_deny").Inc()
			slog.Info("proxy deny", "host", hostname, "reason", "global_deny")
			return goproxy.RejectConnect, host
		}
		if !isAllowed(hostname, allowed) {
			m.deny.WithLabelValues(hostname, "not_in_allowlist").Inc()
			slog.Info("proxy deny", "host", hostname, "reason", "not_in_allowlist")
			return goproxy.RejectConnect, host
		}
		m.allow.WithLabelValues(hostname).Inc()
		slog.Debug("proxy allow", "host", hostname)
		return goproxy.OkConnect, host
	})

	// Plain-HTTP requests are blocked at the proxy layer. /_health and /_metrics
	// are served by the wrapping handler below before requests reach goproxy.
	gp.OnRequest().DoFunc(func(req *http.Request, _ *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		if req.Method != http.MethodConnect {
			slog.Info("proxy deny", "method", req.Method, "url", req.URL.String(), "reason", "non_connect_blocked")
			return req, goproxy.NewResponse(req,
				goproxy.ContentTypeText, http.StatusForbidden,
				"plain HTTP not supported; use HTTPS\n")
		}
		return req, nil
	})

	handler := wrapAdmin(gp, m)

	return &Server{
		cfg:     cfg,
		metrics: m,
		srv: &http.Server{
			Addr:              cfg.ListenAddr,
			Handler:           handler,
			ReadHeaderTimeout: 0,
		},
	}
}

// wrapAdmin routes /_health and /_metrics directly; everything else falls
// through to goproxy. CONNECT requests don't match these paths so they
// transit to the proxy unchanged.
func wrapAdmin(gp http.Handler, m *metrics) http.Handler {
	metricsHandler := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case healthPath:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
				return
			case metricsPath:
				metricsHandler.ServeHTTP(w, r)
				return
			}
		}
		gp.ServeHTTP(w, r)
	})
}

func (s *Server) Run(ctx context.Context) error {
	listenErr := make(chan error, 1)
	go func() {
		slog.Info("kpproxy listening",
			"addr", s.cfg.ListenAddr,
			"allow", len(s.cfg.AllowedHosts),
			"deny", len(s.cfg.DenyHosts),
		)
		listenErr <- s.srv.ListenAndServe()
	}()

	select {
	case err := <-listenErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		return s.srv.Shutdown(context.Background())
	}
}

func splitHostPort(s string) (string, string, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", "", fmt.Errorf("split %q: %w", s, err)
	}
	return host, port, nil
}

func normalize(in []string) []string {
	out := make([]string, 0, len(in))
	for _, h := range in {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			out = append(out, h)
		}
	}
	return out
}

func isAllowed(host string, allow []string) bool {
	if len(allow) == 0 {
		return false
	}
	host = strings.ToLower(host)
	return slices.Contains(allow, host)
}

func isDenied(host string, deny []string) bool {
	host = strings.ToLower(host)
	return slices.Contains(deny, host)
}
