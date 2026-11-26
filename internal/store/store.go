// Package store is the server's Postgres-backed source of truth for product state (docs/architecture.md D6):
// tenants, API keys, sandboxes (lifecycle state machine with CAS + generation fencing), usage events.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// Lifecycle states (OpenSandbox-compatible names).
const (
	Pending    = "Pending"
	Running    = "Running"
	Pausing    = "Pausing"
	Paused     = "Paused"
	Resuming   = "Resuming"
	Stopping   = "Stopping"
	Terminated = "Terminated"
	Failed     = "Failed"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("state conflict") // CAS lost: sandbox not in an allowed source state
	ErrQuota      = errors.New("quota exceeded")
	ErrBadKey     = errors.New("invalid api key")
	ErrDuplicated = errors.New("already exists")
)

type Store struct{ db *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 20
	var db *pgxpool.Pool
	for i := 0; i < 30; i++ { // tolerate Postgres starting after us
		if db, err = pgxpool.NewWithConfig(ctx, cfg); err == nil {
			if err = db.Ping(ctx); err == nil {
				break
			}
			db.Close()
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if _, err := db.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() { s.db.Close() }

// ---------------------------------------------------------------- tenants & keys

func (s *Store) CreateTenant(ctx context.Context, id string, maxRunning int) error {
	_, err := s.db.Exec(ctx, `INSERT INTO tenants (id, max_running) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET max_running = EXCLUDED.max_running`, id, maxRunning)
	return err
}

func hashKey(k string) string { h := sha256.Sum256([]byte(k)); return hex.EncodeToString(h[:]) }

// CreateKey mints a new API key for a tenant. The plaintext is returned once and never stored.
func (s *Store) CreateKey(ctx context.Context, tenant string) (keyID, plaintext string, err error) {
	b := make([]byte, 24)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	idb := make([]byte, 4)
	_, _ = rand.Read(idb)
	keyID = "key_" + hex.EncodeToString(idb)
	plaintext = "sandbox_" + hex.EncodeToString(idb) + "_" + hex.EncodeToString(b)
	_, err = s.db.Exec(ctx, `INSERT INTO api_keys (id, tenant_id, key_hash) VALUES ($1, $2, $3)`,
		keyID, tenant, hashKey(plaintext))
	if err != nil && strings.Contains(err.Error(), "foreign key") {
		return "", "", fmt.Errorf("tenant %q does not exist: %w", tenant, ErrNotFound)
	}
	return keyID, plaintext, err
}

func (s *Store) RevokeKey(ctx context.Context, keyID string) error {
	ct, err := s.db.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, keyID)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

type KeyInfo struct {
	ID        string    `json:"keyId"`
	Tenant    string    `json:"tenant"`
	CreatedAt time.Time `json:"createdAt"`
	Revoked   bool      `json:"revoked"`
}

func (s *Store) ListKeys(ctx context.Context, tenant string) ([]KeyInfo, error) {
	rows, err := s.db.Query(ctx, `SELECT id, tenant_id, created_at, revoked_at IS NOT NULL FROM api_keys
		WHERE ($1 = '' OR tenant_id = $1) ORDER BY created_at`, tenant)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (KeyInfo, error) {
		var k KeyInfo
		return k, r.Scan(&k.ID, &k.Tenant, &k.CreatedAt, &k.Revoked)
	})
}

// Authenticate resolves a plaintext API key to its tenant.
func (s *Store) Authenticate(ctx context.Context, key string) (string, error) {
	var tenant string
	err := s.db.QueryRow(ctx, `SELECT tenant_id FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
		hashKey(key)).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrBadKey
	}
	return tenant, err
}

// ---------------------------------------------------------------- sandboxes

type Sandbox struct {
	ID, Tenant, Cell, Template, Image string
	Entrypoint                        []string
	Env, Metadata                     map[string]string
	Network                           json.RawMessage
	State, StateMessage               string
	Generation                        int64
	ClaimName, PodName, PodIP         *string
	ExecdToken, SnapshotKey           *string
	SnapshotBytes                     *int64
	ExpiresAt                         *time.Time
	CreatedAt, UpdatedAt              time.Time
}

const cols = `id, tenant_id, cell_id, template, image, entrypoint, env, metadata, network, state, state_message,
	generation, claim_name, pod_name, pod_ip, execd_token, snapshot_key, snapshot_bytes, expires_at, created_at, updated_at`

func scan(r pgx.Row) (*Sandbox, error) {
	var sb Sandbox
	var ep, env, md []byte
	err := r.Scan(&sb.ID, &sb.Tenant, &sb.Cell, &sb.Template, &sb.Image, &ep, &env, &md, &sb.Network, &sb.State,
		&sb.StateMessage, &sb.Generation, &sb.ClaimName, &sb.PodName, &sb.PodIP, &sb.ExecdToken, &sb.SnapshotKey,
		&sb.SnapshotBytes, &sb.ExpiresAt, &sb.CreatedAt, &sb.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(ep, &sb.Entrypoint)
	_ = json.Unmarshal(env, &sb.Env)
	_ = json.Unmarshal(md, &sb.Metadata)
	return &sb, nil
}

// InsertWithQuota creates a sandbox row in Pending, atomically enforcing the tenant's running quota.
func (s *Store) InsertWithQuota(ctx context.Context, sb *Sandbox) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var max, running int
	// Lock the tenant row so concurrent creates for one tenant serialize on the quota check.
	if err := tx.QueryRow(ctx, `SELECT max_running FROM tenants WHERE id = $1 FOR UPDATE`, sb.Tenant).Scan(&max); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sandboxes WHERE tenant_id = $1 AND state IN ('Pending','Running','Resuming','Pausing')`,
		sb.Tenant).Scan(&running); err != nil {
		return err
	}
	if running >= max {
		return fmt.Errorf("%w: %d/%d running sandboxes", ErrQuota, running, max)
	}
	ep, _ := json.Marshal(sb.Entrypoint)
	env, _ := json.Marshal(nonNil(sb.Env))
	md, _ := json.Marshal(nonNil(sb.Metadata))
	_, err = tx.Exec(ctx, `INSERT INTO sandboxes (id, tenant_id, cell_id, template, image, entrypoint, env, metadata,
		network, state, expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		sb.ID, sb.Tenant, sb.Cell, sb.Template, sb.Image, ep, env, md, nullJSON(sb.Network), Pending, sb.ExpiresAt)
	if err != nil {
		return err
	}
	if err := event(ctx, tx, sb.ID, sb.Tenant, "created", 0, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func nullJSON(b json.RawMessage) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return []byte(b)
}

func (s *Store) Get(ctx context.Context, tenant, id string) (*Sandbox, error) {
	return scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM sandboxes WHERE id = $1 AND ($2 = '' OR tenant_id = $2)`, id, tenant))
}

func (s *Store) List(ctx context.Context, tenant string, states []string, metadata map[string]string, limit, offset int) ([]*Sandbox, int, error) {
	md, _ := json.Marshal(nonNil(metadata))
	where := `tenant_id = $1 AND (cardinality($2::text[]) = 0 OR state = ANY($2)) AND metadata @> $3::jsonb`
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM sandboxes WHERE `+where, tenant, states, md).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM sandboxes WHERE `+where+` ORDER BY created_at DESC LIMIT $4 OFFSET $5`,
		tenant, states, md, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Sandbox
	for rows.Next() {
		sb, err := scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, sb)
	}
	return out, total, rows.Err()
}

// Patch describes column updates applied together with a state transition.
type Patch struct {
	StateMessage   *string
	BumpGeneration bool
	ClaimName      **string
	PodName        **string
	PodIP          **string
	ExecdToken     **string
	SnapshotKey    **string
	SnapshotBytes  **int64
	ExpiresAt      **time.Time
}

func Str(s string) **string { p := &s; return &p }
func NullStr() **string     { var p *string; return &p }

// Transition atomically moves a sandbox from one of `from` to `to` (compare-and-set) and applies the patch.
// Returns ErrConflict if the sandbox is not currently in an allowed state. Emits a usage event when `ev` != "".
func (s *Store) Transition(ctx context.Context, id string, from []string, to, ev string, p Patch) (*Sandbox, error) {
	set := []string{"state = $3", "updated_at = now()"}
	args := []any{id, from, to}
	add := func(col string, v any) {
		args = append(args, v)
		set = append(set, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if p.StateMessage != nil {
		add("state_message", *p.StateMessage)
	} else {
		add("state_message", "")
	}
	if p.BumpGeneration {
		set = append(set, "generation = generation + 1")
	}
	if p.ClaimName != nil {
		add("claim_name", *p.ClaimName)
	}
	if p.PodName != nil {
		add("pod_name", *p.PodName)
	}
	if p.PodIP != nil {
		add("pod_ip", *p.PodIP)
	}
	if p.ExecdToken != nil {
		add("execd_token", *p.ExecdToken)
	}
	if p.SnapshotKey != nil {
		add("snapshot_key", *p.SnapshotKey)
	}
	if p.SnapshotBytes != nil {
		add("snapshot_bytes", *p.SnapshotBytes)
	}
	if p.ExpiresAt != nil {
		add("expires_at", *p.ExpiresAt)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	sb, err := scan(tx.QueryRow(ctx, `UPDATE sandboxes SET `+strings.Join(set, ", ")+
		` WHERE id = $1 AND state = ANY($2) RETURNING `+cols, args...))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	if ev != "" {
		if err := event(ctx, tx, sb.ID, sb.Tenant, ev, sb.Generation, nil); err != nil {
			return nil, err
		}
	}
	return sb, tx.Commit(ctx)
}

// Expired returns sandboxes whose TTL has passed and that are still holding or able to hold resources.
func (s *Store) Expired(ctx context.Context, limit int) ([]*Sandbox, error) {
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM sandboxes WHERE expires_at < now()
		AND state IN ('Running','Paused','Failed') ORDER BY expires_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Sandbox
	for rows.Next() {
		sb, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// InStates returns all sandboxes currently in one of the given states (startup recovery).
func (s *Store) InStates(ctx context.Context, states []string) ([]*Sandbox, error) {
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM sandboxes WHERE state = ANY($1) ORDER BY updated_at`, states)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Sandbox
	for rows.Next() {
		sb, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// ActiveClaims returns claim names the store believes are live (for two-way reconciliation).
func (s *Store) ActiveClaims(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.Query(ctx, `SELECT claim_name, id FROM sandboxes WHERE claim_name IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var c, id string
		if err := rows.Scan(&c, &id); err != nil {
			return nil, err
		}
		out[c] = id
	}
	return out, rows.Err()
}

func event(ctx context.Context, tx pgx.Tx, id, tenant, ev string, gen int64, detail map[string]any) error {
	d, _ := json.Marshal(detail)
	if detail == nil {
		d = []byte("{}")
	}
	_, err := tx.Exec(ctx, `INSERT INTO usage_events (sandbox_id, tenant_id, event, generation, detail) VALUES ($1,$2,$3,$4,$5)`,
		id, tenant, ev, gen, d)
	return err
}

// Usage returns per-tenant event counts (simple showback).
func (s *Store) Usage(ctx context.Context) (map[string]map[string]int, error) {
	rows, err := s.db.Query(ctx, `SELECT tenant_id, event, count(*) FROM usage_events GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var t, e string
		var n int
		if err := rows.Scan(&t, &e, &n); err != nil {
			return nil, err
		}
		if out[t] == nil {
			out[t] = map[string]int{}
		}
		out[t][e] = n
	}
	return out, rows.Err()
}
