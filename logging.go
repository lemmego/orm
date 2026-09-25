package orm

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Operation distinguishes the two kinds of statement the ORM issues.
type Operation string

const (
	OpQuery Operation = "query"
	OpExec  Operation = "exec"
)

// QueryEvent describes one executed statement.
type QueryEvent struct {
	Operation Operation
	SQL       string
	Args      []any
	Duration  time.Duration
	Err       error

	// Rows is the number of rows affected by an exec, or -1 when the figure
	// is unavailable — which it always is for a query, since rows are counted
	// as they are scanned, long after the statement returns.
	Rows int64
}

// String renders the event as a single readable line.
func (e QueryEvent) String() string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "[%s] %s", e.Duration.Round(time.Microsecond), collapseWhitespace(e.SQL))
	if len(e.Args) > 0 {
		fmt.Fprintf(&builder, " args=%v", e.Args)
	}
	if e.Rows >= 0 {
		fmt.Fprintf(&builder, " rows=%d", e.Rows)
	}
	if e.Err != nil {
		fmt.Fprintf(&builder, " error=%v", e.Err)
	}
	return builder.String()
}

func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// Logger receives every statement the ORM executes, including the extra
// queries eager loading issues.
//
// It is called synchronously on the query path, so a slow logger slows every
// query. Hand off to a channel or a buffered writer if the work is expensive.
type Logger func(context.Context, QueryEvent)

// WithLogger installs a statement logger.
func WithLogger(logger Logger) Option {
	return func(db *DB) { db.logger = logger }
}

// WithSlogLogger logs statements through log/slog, at debug level for
// successes and error level for failures.
func WithSlogLogger(logger *slog.Logger) Option {
	if logger == nil {
		logger = slog.Default()
	}
	return WithLogger(func(ctx context.Context, event QueryEvent) {
		attrs := []any{
			slog.String("sql", collapseWhitespace(event.SQL)),
			slog.Duration("duration", event.Duration),
		}
		if len(event.Args) > 0 {
			attrs = append(attrs, slog.Any("args", event.Args))
		}
		if event.Rows >= 0 {
			attrs = append(attrs, slog.Int64("rows", event.Rows))
		}
		if event.Err != nil {
			logger.ErrorContext(ctx, "orm: "+string(event.Operation), append(attrs, slog.Any("error", event.Err))...)
			return
		}
		logger.DebugContext(ctx, "orm: "+string(event.Operation), attrs...)
	})
}

// loggingExecutor times every statement and reports it.
//
// Wrapping the executor rather than instrumenting each call site means there
// is one place to keep correct, and nothing can be missed — the relation
// loader and the association writers are covered without knowing about it.
type loggingExecutor struct {
	inner  executor
	logger Logger
}

// withLogging wraps an executor when a logger is installed, and returns it
// untouched when none is, so the uninstrumented path costs nothing.
func withLogging(inner executor, logger Logger) executor {
	if logger == nil {
		return inner
	}
	return &loggingExecutor{inner: inner, logger: logger}
}

func (e *loggingExecutor) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	started := time.Now()
	result, err := e.inner.ExecContext(ctx, query, args...)

	rows := int64(-1)
	if err == nil && result != nil {
		if affected, affectedErr := result.RowsAffected(); affectedErr == nil {
			rows = affected
		}
	}
	e.logger(ctx, QueryEvent{
		Operation: OpExec,
		SQL:       query,
		Args:      args,
		Duration:  time.Since(started),
		Rows:      rows,
		Err:       err,
	})
	return result, err
}

func (e *loggingExecutor) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	started := time.Now()
	rows, err := e.inner.QueryContext(ctx, query, args...)

	e.logger(ctx, QueryEvent{
		Operation: OpQuery,
		SQL:       query,
		Args:      args,
		Duration:  time.Since(started),
		Rows:      -1,
		Err:       err,
	})
	return rows, err
}
