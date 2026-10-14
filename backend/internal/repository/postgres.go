package repository

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Postgres implements database operations using PostgreSQL through database/sql.
type Postgres struct {
	db              *sql.DB
	agentTableMu    sync.Mutex
	agentTableReady bool
}

// OpenPostgres creates a PostgreSQL connection pool from a connection URL.
// database/sql establishes network connections lazily when the pool is used.
func OpenPostgres(databaseURL string) (*Postgres, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	return &Postgres{db: db}, nil
}

// Ping verifies that PostgreSQL is reachable within the caller's deadline.
func (p *Postgres) Ping(ctx context.Context) error {
	return p.db.PingContext(ctx)
}

// Close releases the connection pool and its resources.
func (p *Postgres) Close() error {
	return p.db.Close()
}

// LookupAgentThread returns the Codex thread ID mapped to agentName. ctx
// controls the query; agentName is the caller-facing three-letter name. An
// unknown name returns an empty thread ID and no error.
func (p *Postgres) LookupAgentThread(ctx context.Context, agentName string) (string, error) {
	if err := p.ensureAgentThreadTable(ctx); err != nil {
		return "", err
	}
	var threadID string
	err := p.db.QueryRowContext(ctx, `SELECT thread_id FROM codex_agent_threads WHERE agent_name = $1`, agentName).Scan(&threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return threadID, err
}

// CreateAgentThread assigns a unique generated three-letter name to threadID.
// ctx controls database operations; threadID is the Codex conversation ID.
func (p *Postgres) CreateAgentThread(ctx context.Context, threadID string) (string, error) {
	if err := p.ensureAgentThreadTable(ctx); err != nil {
		return "", err
	}
	for attempt := 0; attempt < 64; attempt++ {
		name, err := generateAgentName()
		if err != nil {
			return "", err
		}
		var storedName string
		err = p.db.QueryRowContext(ctx, `
			INSERT INTO codex_agent_threads (agent_name, thread_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING
			RETURNING agent_name`, name, threadID).Scan(&storedName)
		if err == nil {
			return storedName, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}

		// A thread may already have a name if another backend instance stored it.
		err = p.db.QueryRowContext(ctx, `SELECT agent_name FROM codex_agent_threads WHERE thread_id = $1`, threadID).Scan(&storedName)
		if err == nil {
			return storedName, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return "", errors.New("could not allocate a unique agent name after 64 attempts")
}

// ReplaceAgentThread removes the stale mapping for agentName and maps the new
// Codex thread to a fresh name. ctx controls the transaction; agentName and
// oldThreadID identify the stale entry; newThreadID is the replacement thread.
func (p *Postgres) ReplaceAgentThread(ctx context.Context, agentName, oldThreadID, newThreadID string) (string, error) {
	if err := p.ensureAgentThreadTable(ctx); err != nil {
		return "", err
	}
	for attempt := 0; attempt < 64; attempt++ {
		name, err := generateAgentName()
		if err != nil {
			return "", err
		}
		tx, err := p.db.BeginTx(ctx, nil)
		if err != nil {
			return "", err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM codex_agent_threads WHERE agent_name = $1 AND thread_id = $2`, agentName, oldThreadID)
		if err != nil {
			_ = tx.Rollback()
			return "", err
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return "", err
		}
		if deleted == 0 {
			_ = tx.Rollback()
			// Another server may already have replaced the stale entry.
			return p.CreateAgentThread(ctx, newThreadID)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO codex_agent_threads (agent_name, thread_id) VALUES ($1, $2)`, name, newThreadID)
		if err != nil {
			_ = tx.Rollback()
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				continue
			}
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		return name, nil
	}
	return "", errors.New("could not allocate a unique agent name after 64 attempts")
}

// ensureAgentThreadTable creates the mapping table on first use. ctx limits the
// schema check; the mutex prevents concurrent requests from running the DDL
// repeatedly in this backend process.
func (p *Postgres) ensureAgentThreadTable(ctx context.Context) error {
	p.agentTableMu.Lock()
	defer p.agentTableMu.Unlock()
	if p.agentTableReady {
		return nil
	}
	_, err := p.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS codex_agent_threads (
			agent_name TEXT PRIMARY KEY CHECK (agent_name ~ '^[a-z]{3}$'),
			thread_id TEXT NOT NULL UNIQUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`)
	if err != nil {
		return fmt.Errorf("create Codex agent mapping table: %w", err)
	}
	p.agentTableReady = true
	return nil
}

// generateAgentName returns a uniformly sampled three-letter lowercase name.
// It has no parameters; callers use database uniqueness constraints to retry
// the unlikely case where a generated name is already assigned.
func generateAgentName() (string, error) {
	var name [3]byte
	for i := range name {
		letter, err := cryptorand.Int(cryptorand.Reader, big.NewInt(26))
		if err != nil {
			return "", fmt.Errorf("generate agent name: %w", err)
		}
		name[i] = byte('a' + letter.Int64())
	}
	return string(name[:]), nil
}
