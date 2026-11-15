package orm

import (
	"context"
	"log/slog"
	"sync"
)

// ChangeOp is what happened to a row.
//
// Named ChangeOp rather than Op, and the constants ChangeCreate rather than
// OpCreate, because logging.go already has an Operation type whose constants
// are OpQuery and OpExec. Two unrelated things both spelled Op* in one
// package is the kind of near-collision that gets read wrong at a call site
// long after it is written.
type ChangeOp uint8

const (
	ChangeCreate ChangeOp = iota + 1
	ChangeUpdate
	ChangeDelete
)

// String names the operation, for a log line or an error.
func (op ChangeOp) String() string {
	switch op {
	case ChangeCreate:
		return "create"
	case ChangeUpdate:
		return "update"
	case ChangeDelete:
		return "delete"
	}
	return "unknown"
}

// Change is one row's transition, as an observer sees it.
type Change struct {
	// Op is what happened. A soft delete is ChangeDelete and not
	// ChangeUpdate: it is an UPDATE statement, but what it means to
	// everything downstream — a search index, a cache, a feed — is that the
	// row is gone.
	Op ChangeOp

	// Entity is the model, as the hook saw it: the same pointer the caller
	// passed to Create, Update or Delete, so a generated primary key and the
	// timestamps are already on it.
	//
	// `any` because an observer is process-wide and sees every model, so
	// there is no one T to parameterise it with. An observer type-asserts for
	// the models it cares about, which is also what keeps it from having to
	// know about the rest.
	Entity any

	// Table is the relation the statement named, after any Table() override —
	// so an observer that keys on it sees the row where it actually went,
	// not where the model's metadata says it lives.
	Table string
}

// Observe registers an observer, called after a successful write.
//
// Additive, like tasker.AddGlobalMiddleware: every observer registered sees
// every change, and there is no way to remove one. Registration is usually a
// boot-time act.
//
// A method on *DB rather than a package-level registry, and that is not
// stylistic. A package registry fires for every handle in the process,
// cannot be cleaned up between test cases, and so lets one case's observer
// satisfy the next case's assertion — a global registry that manufactures a
// vacuous test. This project has found that class of failure often enough to
// design against it.
//
// # Inside a transaction, it fires after the commit
//
// This is the whole value, and it is what GORM's callbacks and Bun's query
// hooks cannot do: both fire *inside* the transaction. A change announced
// there is announced before the row is visible to anything else — so a
// subscriber that refetches finds nothing — and if the transaction then rolls
// back, something has been told about a row that does not exist.
//
// So a change raised through db.Model[T]() fires immediately, and one raised
// through tx.Model[T]() is held until CommitContext succeeds and discarded by
// Rollback. Nothing the caller writes makes the difference; the handle does.
//
// # What an observer must not be
//
// Not the place a counter is incremented. Two concurrent observers both read
// 41 and both write 42, and the lost vote is silent and permanent — that is
// what a database trigger or `SET c = c + 1` is for.
//
// Not a place to do slow work, either: an observer registered on *DB runs on
// the goroutine that did the write, so anything expensive is paid by the
// request. Hand off to a queue.
//
// A panicking observer does not take the write down — each runs under
// recover(), the way cache and notify already do and the core event emitter
// does not — and the panic is logged rather than swallowed.
func (db *DB) Observe(fn func(context.Context, Change)) {
	if fn == nil {
		return
	}
	db.observerMu.Lock()
	defer db.observerMu.Unlock()
	db.observers = append(db.observers, fn)
}

// Observers reports how many are registered, which is what a test asserts
// against rather than reaching into the field.
func (db *DB) Observers() int {
	db.observerMu.RLock()
	defer db.observerMu.RUnlock()
	return len(db.observers)
}

// changeSink is where a write announces itself.
//
// An interface rather than a *Tx pointer on Query[T], because the two
// destinations are genuinely different: a DB dispatches now, a Tx holds until
// commit. It is also why the field is needed at all — Query[T].executor is
// the standard library's *sql.DB or *sql.Tx, not orm.Tx, so there is nothing
// on a query to recover the owning transaction from.
type changeSink interface {
	emit(ctx context.Context, change Change)
}

// emit on a DB dispatches immediately: there is no transaction to wait for.
func (db *DB) emit(ctx context.Context, change Change) { db.dispatch(ctx, change) }

// emit on a Tx holds the change until the commit succeeds.
func (tx *Tx) emit(_ context.Context, change Change) {
	tx.changes = append(tx.changes, change)
}

// dispatch calls every observer, each under its own recover().
func (db *DB) dispatch(ctx context.Context, changes ...Change) {
	db.observerMu.RLock()
	observers := db.observers
	db.observerMu.RUnlock()

	if len(observers) == 0 {
		return
	}
	for _, change := range changes {
		for _, observe := range observers {
			call(ctx, observe, change)
		}
	}
}

// call runs one observer, surviving a panic in it.
func call(ctx context.Context, observe func(context.Context, Change), change Change) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// Logged rather than swallowed: an observer that panics on every
			// write is a bug, and one that is silently skipped forever is a
			// stale index nobody can account for.
			slog.ErrorContext(ctx, "orm: observer panicked",
				"op", change.Op.String(), "table", change.Table,
				"panic", recovered)
		}
	}()
	observe(ctx, change)
}

// observe announces a change from a query, if anything is listening.
//
// Called beside each AfterCreate, AfterUpdate and AfterDelete hook, so there
// are no new places a write can happen — and so an observer sees exactly
// what an entity hook sees.
func (q *Query[T]) observe(ctx context.Context, op ChangeOp, entity any) {
	if q.changeSink == nil {
		return
	}
	q.changeSink.emit(ctx, Change{Op: op, Entity: entity, Table: q.table})
}

// observedBy attaches a sink, returning the same query for chaining at a
// constructor.
func (q *Query[T]) observedBy(sink changeSink) *Query[T] {
	q.changeSink = sink
	return q
}

// observerState is embedded in DB.
type observerState struct {
	observerMu sync.RWMutex
	observers  []func(context.Context, Change)
}
