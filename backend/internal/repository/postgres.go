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
	authTableMu     sync.Mutex
	authTableReady  bool
}

// UserRecord contains the account fields required by authentication storage.
type UserRecord struct {
	ID    int64
	Email string
	Role  string
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

// CreateUser inserts an account with a pre-hashed password and returns its public fields.
// ctx bounds the query; email is normalized by the service and passwordHash is a bcrypt digest.
func (p *Postgres) CreateUser(ctx context.Context, email, passwordHash string) (int64, string, error) {
	if err := p.ensureAuthTables(ctx); err != nil {
		return 0, "", err
	}
	var user UserRecord
	err := p.db.QueryRowContext(ctx, `INSERT INTO app_users (email, password_hash) VALUES ($1, $2) RETURNING id, email`, email, passwordHash).Scan(&user.ID, &user.Email)
	return user.ID, user.Email, err
}

// FindUserByEmail returns an account and its password hash for credential verification.
// ctx bounds the query; email is the normalized address to look up.
func (p *Postgres) FindUserByEmail(ctx context.Context, email string) (int64, string, string, string, error) {
	if err := p.ensureAuthTables(ctx); err != nil {
		return 0, "", "", "", err
	}
	var user UserRecord
	var passwordHash string
	err := p.db.QueryRowContext(ctx, `SELECT id, email, role, password_hash FROM app_users WHERE email = $1`, email).Scan(&user.ID, &user.Email, &user.Role, &passwordHash)
	return user.ID, user.Email, user.Role, passwordHash, err
}

// ensureAuthTables creates or upgrades the account table once per repository instance.
// ctx bounds the DDL operation; the mutex prevents concurrent first-use creation attempts.
func (p *Postgres) ensureAuthTables(ctx context.Context) error {
	p.authTableMu.Lock()
	defer p.authTableMu.Unlock()
	if p.authTableReady {
		return nil
	}
	_, err := p.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS app_users (
			id BIGSERIAL PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'user',
			created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`)
	if err == nil {
		_, err = p.db.ExecContext(ctx, `ALTER TABLE app_users ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'user'`)
	}
	if err != nil {
		return fmt.Errorf("create authentication tables: %w", err)
	}
	p.authTableReady = true
	return nil
}

// LookupAgentThread returns the Codex thread ID mapped to agentName. ctx
// controls the query; userID owns the alias and agentName is its three-letter
// name. An unknown or differently owned name returns an empty ID and no error.
func (p *Postgres) LookupAgentThread(ctx context.Context, userID int64, agentName string) (string, error) {
	if err := p.ensureAgentThreadTable(ctx); err != nil {
		return "", err
	}
	var threadID string
	err := p.db.QueryRowContext(ctx, `SELECT thread_id FROM codex_agent_threads WHERE user_id = $1 AND agent_name = $2`, userID, agentName).Scan(&threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return threadID, err
}

// CreateAgentThread assigns a unique generated three-letter name to threadID.
// ctx controls database operations; userID owns the mapping and threadID is the Codex conversation ID.
func (p *Postgres) CreateAgentThread(ctx context.Context, userID int64, threadID string) (string, error) {
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
			INSERT INTO codex_agent_threads (agent_name, thread_id, user_id)
			VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING
			RETURNING agent_name`, name, threadID, userID).Scan(&storedName)
		if err == nil {
			return storedName, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}

		// A thread may already have a name if another backend instance stored it.
		err = p.db.QueryRowContext(ctx, `SELECT agent_name FROM codex_agent_threads WHERE thread_id = $1 AND user_id = $2`, threadID, userID).Scan(&storedName)
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
// Codex thread to a fresh name. ctx controls the transaction; userID owns the
// mapping; agentName and oldThreadID identify the stale entry; newThreadID is the replacement thread.
func (p *Postgres) ReplaceAgentThread(ctx context.Context, userID int64, agentName, oldThreadID, newThreadID string) (string, error) {
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
		result, err := tx.ExecContext(ctx, `DELETE FROM codex_agent_threads WHERE user_id = $1 AND agent_name = $2 AND thread_id = $3`, userID, agentName, oldThreadID)
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
			return p.CreateAgentThread(ctx, userID, newThreadID)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO codex_agent_threads (agent_name, thread_id, user_id) VALUES ($1, $2, $3)`, name, newThreadID, userID)
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
	if err := p.ensureAuthTables(ctx); err != nil {
		return err
	}
	p.agentTableMu.Lock()
	defer p.agentTableMu.Unlock()
	if p.agentTableReady {
		return nil
	}
	_, err := p.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS codex_agent_threads (
			agent_name TEXT PRIMARY KEY CHECK (agent_name ~ '^[a-z]{3}$'),
			thread_id TEXT NOT NULL UNIQUE,
			user_id BIGINT REFERENCES app_users(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`)
	if err == nil {
		_, err = p.db.ExecContext(ctx, `ALTER TABLE codex_agent_threads ADD COLUMN IF NOT EXISTS user_id BIGINT REFERENCES app_users(id) ON DELETE CASCADE`)
	}
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
