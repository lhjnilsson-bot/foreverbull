package test_helper //nolint:stylecheck,revive

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// FailingRowsConn answers every Query with rows that end at once with err,
// the way pgx reports a connection lost in the middle of a result. A
// repository that skips rows.Err() returns an empty list as a success.
type FailingRowsConn struct {
	Err error
}

func (c FailingRowsConn) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return failingRows{err: c.Err}, nil
}

func (c FailingRowsConn) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return failingRows{err: c.Err}
}

func (c FailingRowsConn) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, c.Err
}

type failingRows struct {
	err error
}

func (r failingRows) Close()                                       {}
func (r failingRows) Err() error                                   { return r.err }
func (r failingRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r failingRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r failingRows) Next() bool                                   { return false }
func (r failingRows) Scan(...any) error                            { return r.err }
func (r failingRows) Values() ([]any, error)                       { return nil, r.err }
func (r failingRows) RawValues() [][]byte                          { return nil }
func (r failingRows) Conn() *pgx.Conn                              { return nil }
func (r failingRows) TypeMap() *pgtype.Map                         { return nil }
