package service

import (
	"errors"

	"github.com/lhjnilsson/foreverbull/internal/postgres"
	"github.com/lhjnilsson/foreverbull/pkg/service/internal/repository"
)

// NewStore returns the service table behind GET and POST /api/services.
// The image is the primary key. Callers outside this module use it instead of
// the internal repository.
func NewStore(conn postgres.Query) ServiceStore {
	return &repository.Service{Conn: conn}
}

// ErrNotFound is a service image with no row. A store's other errors are
// read failures, not "not found".
var ErrNotFound = repository.ErrServiceNotFound

// IsNotFound reports a missing image: ErrNotFound, not pgx.ErrNoRows.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}
