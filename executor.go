package orm

import (
	"context"
	"database/sql"
)

// executor is the minimal contract the query compiler needs.
//
// QueryRowContext is deliberately absent: *sql.Row hides its error until Scan,
// which makes it impossible to report failures to a logger. Every read goes
// through QueryContext instead, so instrumentation sees all of them.
type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

var _ executor = (*sql.DB)(nil)
var _ executor = (*sql.Tx)(nil)
