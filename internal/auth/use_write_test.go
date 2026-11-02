package auth

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// rowDB answers QueryRow with one row of values and Exec with execErr.
type rowDB struct {
	panicDB
	values  []any
	execErr error
}

type valuesRow []any

func (r valuesRow) Scan(dest ...any) error {
	for i, d := range dest {
		target := reflect.ValueOf(d).Elem()
		if r[i] == nil {
			target.Set(reflect.Zero(target.Type()))
			continue
		}
		target.Set(reflect.ValueOf(r[i]))
	}
	return nil
}

func (d rowDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return valuesRow(d.values)
}

func (d rowDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, d.execErr
}

// A login refresh that can't be written fails the sign-in with the cause.
func TestGitHubLoginRefreshFails(t *testing.T) {
	ctx := context.Background()
	broken := errors.New("connection lost")

	t.Run("github login refresh", func(t *testing.T) {
		h := &handlers{db: rowDB{values: []any{"acc-1", RoleEditor}, execErr: broken}}
		_, err := h.upsertGitHub(ctx, githubIdentity{Subject: "42", Login: "renamed"})
		require.ErrorIs(t, err, broken)
		require.EqualError(t, err, "refresh github login: connection lost")
	})
}
