package sandbox

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("sandbox not found")

type Store interface {
	Put(ctx context.Context, s Sandbox) error
	Get(ctx context.Context, id string) (Sandbox, error)
	List(ctx context.Context) ([]Sandbox, error)
	Delete(ctx context.Context, id string) error
	ExpiredBefore(ctx context.Context, t time.Time) ([]Sandbox, error)
	Close() error
}
