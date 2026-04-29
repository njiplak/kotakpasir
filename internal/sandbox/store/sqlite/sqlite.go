package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"nexteam.id/kotakpasir/internal/sandbox"
)

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sandboxes (
    id           TEXT PRIMARY KEY,
    name         TEXT,
    image        TEXT NOT NULL,
    runtime_id   TEXT,
    state        TEXT NOT NULL,
    cpus         REAL,
    memory_mb    INTEGER,
    env_json     TEXT,
    labels_json  TEXT,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_sandboxes_state   ON sandboxes(state);
CREATE INDEX IF NOT EXISTS idx_sandboxes_expires ON sandboxes(expires_at);
`

func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL;"); err != nil {
		return nil, fmt.Errorf("set wal: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON;"); err != nil {
		return nil, fmt.Errorf("set foreign_keys: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Ping verifies the database is reachable. Cheap; safe to call from /healthz.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) Put(ctx context.Context, sb sandbox.Sandbox) error {
	envJSON, err := json.Marshal(sb.Env)
	if err != nil {
		return err
	}
	labelsJSON, err := json.Marshal(sb.Labels)
	if err != nil {
		return err
	}
	var expires sql.NullInt64
	if sb.ExpiresAt != nil {
		expires = sql.NullInt64{Int64: sb.ExpiresAt.Unix(), Valid: true}
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO sandboxes (id, name, image, runtime_id, state, cpus, memory_mb, env_json, labels_json, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    name=excluded.name,
    image=excluded.image,
    runtime_id=excluded.runtime_id,
    state=excluded.state,
    cpus=excluded.cpus,
    memory_mb=excluded.memory_mb,
    env_json=excluded.env_json,
    labels_json=excluded.labels_json,
    expires_at=excluded.expires_at
`,
		sb.ID, sb.Name, sb.Image, sb.RuntimeID, string(sb.State),
		sb.Cpus, sb.MemoryMB, string(envJSON), string(labelsJSON),
		sb.CreatedAt.Unix(), expires,
	)
	return err
}

func (s *Store) Get(ctx context.Context, id string) (sandbox.Sandbox, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, name, image, runtime_id, state, cpus, memory_mb, env_json, labels_json, created_at, expires_at
FROM sandboxes WHERE id = ?`, id)
	sb, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return sandbox.Sandbox{}, sandbox.ErrNotFound
	}
	return sb, err
}

func (s *Store) List(ctx context.Context) ([]sandbox.Sandbox, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, image, runtime_id, state, cpus, memory_mb, env_json, labels_json, created_at, expires_at
FROM sandboxes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []sandbox.Sandbox
	for rows.Next() {
		sb, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

func (s *Store) ExpiredBefore(ctx context.Context, t time.Time) ([]sandbox.Sandbox, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, image, runtime_id, state, cpus, memory_mb, env_json, labels_json, created_at, expires_at
FROM sandboxes
WHERE expires_at IS NOT NULL AND expires_at < ?
ORDER BY expires_at ASC`, t.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []sandbox.Sandbox
	for rows.Next() {
		sb, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sandboxes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sandbox.ErrNotFound
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(s scanner) (sandbox.Sandbox, error) {
	var (
		sb         sandbox.Sandbox
		state      string
		envJSON    sql.NullString
		labelsJSON sql.NullString
		createdAt  int64
		expiresAt  sql.NullInt64
		cpus       sql.NullFloat64
		memoryMB   sql.NullInt64
		runtimeID  sql.NullString
		name       sql.NullString
	)
	if err := s.Scan(&sb.ID, &name, &sb.Image, &runtimeID, &state, &cpus, &memoryMB, &envJSON, &labelsJSON, &createdAt, &expiresAt); err != nil {
		return sb, err
	}
	sb.Name = name.String
	sb.RuntimeID = runtimeID.String
	sb.State = sandbox.State(state)
	sb.Cpus = cpus.Float64
	sb.MemoryMB = memoryMB.Int64
	sb.CreatedAt = time.Unix(createdAt, 0).UTC()
	if expiresAt.Valid {
		t := time.Unix(expiresAt.Int64, 0).UTC()
		sb.ExpiresAt = &t
	}
	if envJSON.Valid && envJSON.String != "" {
		_ = json.Unmarshal([]byte(envJSON.String), &sb.Env)
	}
	if labelsJSON.Valid && labelsJSON.String != "" {
		_ = json.Unmarshal([]byte(labelsJSON.String), &sb.Labels)
	}
	return sb, nil
}
