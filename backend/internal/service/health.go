package service

import "context"

// Database is the repository capability required by health checks.
type Database interface {
	Ping(context.Context) error
}

// HealthService contains application-level health and readiness behavior.
type HealthService struct {
	database Database
}

// NewHealthService constructs the health service with its database dependency.
func NewHealthService(database Database) *HealthService {
	return &HealthService{database: database}
}

// CheckDatabase reports whether the configured database can answer a ping.
func (s *HealthService) CheckDatabase(ctx context.Context) error {
	return s.database.Ping(ctx)
}
