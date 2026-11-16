package orm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
)

// Multi-tenancy, as an implicit predicate.
//
// A tenant-scoped model is one carrying a column that says which tenant a row
// belongs to, declared with `orm:"tenantKey"`. Every statement this package
// builds for such a model is filtered by that column, the way a soft-deletable
// model is filtered by its deleted_at — and for the same reason: scoping that
// depends on the caller remembering to write a predicate is scoping that is
// one forgotten WHERE away from showing one tenant another tenant's rows.
//
// # It fails closed
//
// The tenant value rides on the handle, not on the context, because Query[T]
// carries no context.Context — the ctx arrives at execution, long after the
// predicates are decided. So a tenant-scoped model can only be queried through
// a handle that has been bound to a tenant, and querying one through an
// unbound handle is ErrNoTenant rather than an unfiltered result set.
//
// That is the opposite of the usual arrangement, where a missing tenant means
// no filter. A scope that fails open is indistinguishable from working
// software right up to the moment somebody reads a row they should not have.
//
// # This is not the only layer, and should not be the only layer
//
// An implicit predicate cannot reach hand-written SQL: Raw gets no filter,
// because there is nowhere to put one. Use PostgreSQL row-level security as
// well, keyed on the same value this binding carries, and treat this layer as
// the one that gives you a good error message rather than the one that makes
// the guarantee.
var (
	// ErrNoTenant means a tenant-scoped model was reached through a handle
	// with no tenant bound. Returned at construction, so every terminal
	// method on the query reports it.
	ErrNoTenant = errors.New("orm: tenant-scoped model queried through an unbound handle")

	// ErrTenantMismatch means an entity carrying one tenant was written
	// through a handle bound to another.
	ErrTenantMismatch = errors.New("orm: entity belongs to a different tenant")
)

// TenantBinding is the tenant a handle is bound to.
type TenantBinding struct {
	// Value is written to the tenant column and compared against it. Any
	// type database/sql can bind: an integer id, a string, a UUID.
	Value any
}

// tenantExecutor carries a binding alongside the connection.
//
// Wrapping the executor rather than threading a parameter is what makes this
// change small: Tx.executor() is the single point every query, every relation
// handle and every eager load is built from, so a binding attached here
// reaches all of them without another signature moving.
type tenantExecutor struct {
	executor
	binding *TenantBinding
}

// bindTenant returns an executor carrying the binding, or the executor
// unchanged when there is nothing to bind.
func bindTenant(exec executor, binding *TenantBinding) executor {
	if binding == nil || binding.Value == nil {
		return exec
	}
	return tenantExecutor{executor: exec, binding: binding}
}

// tenantOf returns the binding an executor carries, or nil.
func tenantOf(exec executor) *TenantBinding {
	if bound, ok := exec.(tenantExecutor); ok {
		return bound.binding
	}
	return nil
}

// TenantEnter is called once when a tenant-bound transaction opens, before
// any statement runs inside it.
//
// It exists so the database can be told which tenant this transaction is for
// — in PostgreSQL, SELECT set_config('...', value, true), which is
// transaction-local and therefore resets on commit or rollback and cannot
// leak to the next user of a pooled connection. This package deliberately
// does not know what that statement looks like: the GUC name, and whether
// there is row-level security at all, belong to the application.
type TenantEnter func(ctx context.Context, tx *Tx, value any) error

// WithTenantEnter installs the hook above.
func WithTenantEnter(fn TenantEnter) Option {
	return func(db *DB) { db.tenantEnter = fn }
}

// TenantTx runs fn inside a transaction bound to one tenant.
//
// Every query built from the *Tx is filtered by the tenant column, and the
// TenantEnter hook — when one is installed — has already run, so the database
// agrees with the ORM about whose rows these are.
func (db *DB) TenantTx(ctx context.Context, value any, fn func(*Tx) error) error {
	return db.TenantTxOptions(ctx, value, nil, fn)
}

// TenantTxOptions is TenantTx with transaction options. Pass
// &sql.TxOptions{ReadOnly: true} for a request that only reads: it costs
// nothing and turns "a read path wrote something" into a database error.
func (db *DB) TenantTxOptions(ctx context.Context, value any, opts *sql.TxOptions, fn func(*Tx) error) error {
	if value == nil {
		return fmt.Errorf("%w: TenantTx needs a tenant", ErrNoTenant)
	}

	tx, err := db.db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}

	ormTx := &Tx{db: db, tx: tx, tenant: &TenantBinding{Value: value}}
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = ormTx.Rollback()
			panic(recovered)
		}
	}()

	if db.tenantEnter != nil {
		if err := db.tenantEnter(ctx, ormTx, value); err != nil {
			_ = ormTx.Rollback()
			return err
		}
	}

	if err := fn(ormTx); err != nil {
		_ = ormTx.Rollback()
		return err
	}
	return ormTx.CommitContext(ctx)
}

// TenantValue returns the tenant this transaction is bound to, if any.
func (tx *Tx) TenantValue() (any, bool) {
	if tx.tenant == nil {
		return nil, false
	}
	return tx.tenant.Value, true
}

// sameTenant compares a value read off an entity with the bound one.
//
// Types must match exactly. A silent conversion between, say, int and uint64
// is how a zero gets written into a tenant column and a row becomes visible
// to nobody — or to everybody, depending on the policy.
func sameTenant(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if reflect.TypeOf(a) != reflect.TypeOf(b) {
		return false
	}
	return a == b
}
