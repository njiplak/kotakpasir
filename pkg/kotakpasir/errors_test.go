package kotakpasir_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"nexteam.id/kotakpasir/pkg/kotakpasir"
)

func TestClient_ErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"NotFound", http.StatusNotFound, `{"error":"sandbox not found"}`, kotakpasir.ErrNotFound},
		{"Unauthorized", http.StatusUnauthorized, `{"error":"unauthorized"}`, kotakpasir.ErrUnauthorized},
		{"Forbidden", http.StatusForbidden, `{"error":"forbidden"}`, kotakpasir.ErrUnauthorized},
		{"PolicyAllowlist", http.StatusBadRequest, `{"error":"image \"ubuntu\" not in policy.images allowlist"}`, kotakpasir.ErrPolicyDenied},
		{"PolicyProfile", http.StatusBadRequest, `{"error":"profile \"ghost\" not found"}`, kotakpasir.ErrPolicyDenied},
		{"BadRequestPlain", http.StatusBadRequest, `{"error":"cmd is required"}`, kotakpasir.ErrBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := kotakpasir.New(srv.URL)
			_, err := c.Get(context.Background(), "any")

			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v\nwant errors.Is(err, %v) = true", err, tc.want)
			}

			var apiErr *kotakpasir.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("err=%v: errors.As(*Error) = false", err)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("StatusCode=%d, want %d", apiErr.StatusCode, tc.status)
			}
			if apiErr.Message == "" {
				t.Errorf("Message empty")
			}
		})
	}
}

func TestClient_ErrorMessageFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"sandbox 12345 not found"}`))
	}))
	defer srv.Close()

	c := kotakpasir.New(srv.URL)
	_, err := c.Get(context.Background(), "12345")
	if err == nil {
		t.Fatal("want error")
	}
	got := err.Error()
	if got != "kotakpasir: not found (sandbox 12345 not found)" {
		t.Errorf("Error() = %q, want %q", got, "kotakpasir: not found (sandbox 12345 not found)")
	}
}
