package repository

import (
	"context"
	"database/sql"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Postgres implements database operations using PostgreSQL through database/sql.
type Postgres struct {
	db *sql.DB
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
