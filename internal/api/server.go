package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/valyala/fasthttp/fasthttpadaptor"

	"nexteam.id/kotakpasir/internal/sandbox"
)

type Server struct {
	mgr     *sandbox.Manager
	token   string
	metrics http.Handler
}

type Options struct {
	Manager *sandbox.Manager
	Token   string
	// Metrics, if non-nil, is exposed at GET /_metrics (Prometheus format).
	// Unauthenticated by design — typical scrape from prometheus binds to localhost
	// or a private network. Run kpd behind a reverse proxy to gate it externally.
	Metrics http.Handler
}

func NewServer(opts Options) *Server {
	return &Server{mgr: opts.Manager, token: opts.Token, metrics: opts.Metrics}
}

func (s *Server) App() *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:       "kotakpasir",
		StrictRouting: true,
	})

	app.Use(requestid.New())
	app.Use(recover.New())
	app.Use(logger.New(logger.Config{
		Format: "${time} ${method} ${path} ${status} ${latency}\n",
	}))
	app.Use(s.authMiddleware)

	app.Get("/healthz", s.handleHealth)
	if s.metrics != nil {
		// Bridge net/http promhttp into fasthttp via the standard adaptor.
		fhh := fasthttpadaptor.NewFastHTTPHandler(s.metrics)
		app.Get("/_metrics", func(c fiber.Ctx) error {
			fhh(c.RequestCtx())
			return nil
		})
	}

	v1 := app.Group("/v1")
	v1.Post("/sandboxes", s.handleCreate)
	v1.Get("/sandboxes", s.handleList)
	v1.Get("/sandboxes/:id", s.handleGet)
	v1.Get("/sandboxes/:id/proxy", s.handleProxy)
	v1.Get("/sandboxes/:id/logs", s.handleLogs)
	v1.Delete("/sandboxes/:id", s.handleDelete)
	v1.Post("/sandboxes/:id/stop", s.handleStop)
	v1.Post("/sandboxes/:id/exec", s.handleExec)
	v1.Post("/sandboxes/:id/exec/stream", s.handleExecStream)

	return app
}

func (s *Server) authMiddleware(c fiber.Ctx) error {
	if s.token == "" || c.Path() == "/healthz" || c.Path() == "/_metrics" {
		return c.Next()
	}
	got := c.Get(fiber.HeaderAuthorization)
	if !strings.HasPrefix(got, "Bearer ") || strings.TrimPrefix(got, "Bearer ") != s.token {
		slog.Warn("unauthorized", "path", c.Path(), "ip", c.IP())
		return c.Status(fiber.StatusUnauthorized).JSON(ErrorResponse{Error: "unauthorized"})
	}
	return c.Next()
}
