package orm

import (
	"context"
	"database/sql"
)

type DB struct {
	db      *sql.DB
	dialect Dialect
	logger  Logger
}

type Option func(*DB)

func WithDialect(dialect Dialect) Option {
	return func(db *DB) { db.dialect = dialect }
}

func Open(db *sql.DB, dialect Dialect, options ...Option) *DB {
	ormDB := &DB{db: db, dialect: dialect}
	for _, option := range options {
		option(ormDB)
	}
	return ormDB
}

func (db *DB) SQLDB() *sql.DB { return db.db }

func (db *DB) Dialect() Dialect { return db.dialect }

// Model starts a typed query. Raw starts a typed hand-written query.
//
// These are generic methods, which Go 1.27 permits on concrete types. That is
// why DB and Tx are concrete and why there is no shared interface over them:
// a method with its own type parameters can never satisfy an interface, so a
// Querier abstraction carrying Model[T] would not compile.
func (db *DB) Model[T any]() *Query[T] {
	return newQuery[T](db, db.executor())
}

// executor returns the connection, wrapped for logging when one is installed.
func (db *DB) executor() executor { return withLogging(db.db, db.logger) }

// Logger returns the installed statement logger, or nil.
func (db *DB) Logger() Logger { return db.logger }

func (db *DB) Raw[T any](query string, args ...any) *RawQuery[T] {
	return &RawQuery[T]{executor: db.executor(), dialect: db.dialect, query: query, args: append([]any(nil), args...)}
}

func (db *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	result, err := db.executor().ExecContext(ctx, query, args...)
	if err != nil {
		return nil, db.dialect.ClassifyError(err)
	}
	return result, nil
}

func (db *DB) Close() error { return db.db.Close() }

// Transaction runs fn inside a transaction, rolling back on error or panic.
// It is deliberately not bound to one entity type, so a single transaction can
// span as many models as the unit of work needs.
func (db *DB) Transaction(ctx context.Context, fn func(*Tx) error) error {
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	ormTx := &Tx{db: db, tx: tx}
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = ormTx.Rollback()
			panic(recovered)
		}
	}()
	if err := fn(ormTx); err != nil {
		_ = ormTx.Rollback()
		return err
	}
	return ormTx.CommitContext(ctx)
}

// Begin starts a transaction the caller is responsible for finishing.
//
// Transaction is the safer choice and should be preferred: it commits, rolls
// back and handles panics for you. Begin exists for callers that must hand the
// handle around, such as an adapter implementing an external contract.
func (db *DB) Begin(ctx context.Context) (*Tx, error) {
	return db.BeginTx(ctx, nil)
}

func (db *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := db.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{db: db, tx: tx}, nil
}

type Tx struct {
	db          *DB
	tx          *sql.Tx
	closed      bool
	afterCommit []func(context.Context) error
}

// Model and Raw mirror the DB methods so a transaction is a drop-in
// replacement for the connection in any unit of work.
func (tx *Tx) Model[T any]() *Query[T] {
	return newQuery[T](tx.db, tx.executor())
}

// executor returns the transaction, wrapped for logging when one is installed.
func (tx *Tx) executor() executor { return withLogging(tx.tx, tx.db.logger) }

func (tx *Tx) Raw[T any](query string, args ...any) *RawQuery[T] {
	return &RawQuery[T]{executor: tx.executor(), dialect: tx.db.dialect, query: query, args: append([]any(nil), args...)}
}

func (tx *Tx) Dialect() Dialect { return tx.db.dialect }

func (tx *Tx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if tx.closed {
		return nil, ErrClosed
	}
	result, err := tx.executor().ExecContext(ctx, query, args...)
	if err != nil {
		return nil, tx.db.dialect.ClassifyError(err)
	}
	return result, nil
}

func (tx *Tx) Commit() error {
	return tx.CommitContext(context.Background())
}

func (tx *Tx) CommitContext(ctx context.Context) error {
	if tx.closed {
		return ErrClosed
	}
	tx.closed = true
	if err := tx.tx.Commit(); err != nil {
		return err
	}
	for _, callback := range tx.afterCommit {
		if err := callback(ctx); err != nil {
			return err
		}
	}
	return nil
}

// AfterCommit registers a callback that runs only once the database commit
// has succeeded. It cannot roll the transaction back.
func (tx *Tx) AfterCommit(callback func(context.Context) error) error {
	if tx.closed {
		return ErrClosed
	}
	if callback != nil {
		tx.afterCommit = append(tx.afterCommit, callback)
	}
	return nil
}

// Savepoint marks a point inside the transaction that RollbackTo can return
// to, allowing part of a unit of work to be undone without discarding all of
// it.
//
// Savepoint names cannot be bound as parameters, so they are validated as
// plain identifiers rather than interpolated blindly.
func (tx *Tx) Savepoint(ctx context.Context, name string) error {
	return tx.savepointStatement(ctx, "SAVEPOINT ", name)
}

// RollbackTo undoes everything done since the named savepoint.
func (tx *Tx) RollbackTo(ctx context.Context, name string) error {
	return tx.savepointStatement(ctx, "ROLLBACK TO SAVEPOINT ", name)
}

// ReleaseSavepoint discards a savepoint, keeping the work done since it.
func (tx *Tx) ReleaseSavepoint(ctx context.Context, name string) error {
	return tx.savepointStatement(ctx, "RELEASE SAVEPOINT ", name)
}

func (tx *Tx) savepointStatement(ctx context.Context, keyword, name string) error {
	if tx.closed {
		return ErrClosed
	}
	if err := CheckIdentifier(name); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, keyword+tx.db.dialect.Quote(name))
	return err
}

func (tx *Tx) Rollback() error {
	if tx.closed {
		return nil
	}
	tx.closed = true
	return tx.tx.Rollback()
}
