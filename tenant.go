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

// ErrTenantSettingUnsupported means the dialect cannot bind a tenant to a
// transaction, so the database has no way to agree with the ORM about whose
// rows these are.
//
// Returned rather than ignored. An application that wanted row-level security
// and is running on a database without transaction-local settings has one
// isolation layer where it believed it had two, and the only honest moment to
// say so is the first time it tries.
var ErrTenantSettingUnsupported = errors.New("orm: this dialect cannot bind a tenant to a transaction")

// TenantEnterStatement returns the statement that binds a tenant for the rest
// of a transaction, and whether this dialect has one.
//
// The type assertion lives here so there is exactly one of it. A caller that
// wants to refuse at boot rather than at the first request asks this
// question during start-up, which is what a multi-tenancy layer should do:
// "this database cannot enforce what you asked for" is a configuration error,
// not a runtime one.
func TenantEnterStatement(dialect Dialect) (string, bool) {
	enterer, ok := dialect.(TenantEnterDialect)
	if !ok {
		return "", false
	}
	return enterer.TenantEnterSQL(), true
}

// SetTenantSetting returns a TenantEnter that binds the transaction's tenant
// to a transaction-local setting of the given name.
//
// This is what keeps vendor SQL out of application code. Before it, every
// multi-tenant application wrote its own `SELECT set_config($1, $2, true)` at
// a call site — one more place to pass false by mistake, and one more place
// that only works on PostgreSQL without saying so.
//
//	orm.Open(db, orm.Postgres(), orm.WithTenantEnter(
//	    orm.SetTenantSetting("acme.tenant_id")))
//
// The setting's name belongs to the caller: this package does not know
// whether there is row-level security reading it, or what it is called.
func SetTenantSetting(setting string) TenantEnter {
	return func(ctx context.Context, tx *Tx, value any) error {
		query, ok := TenantEnterStatement(tx.Dialect())
		if !ok {
			return fmt.Errorf("%w: %s", ErrTenantSettingUnsupported, tx.Dialect().Name())
		}
		text, err := tenantSettingText(value)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, query, setting, text)
		return err
	}
}

// tenantSettingText renders a tenant key as the text a session setting holds.
//
// A setting is text, and every tenant key type has to arrive as the spelling
// the database will compare against. The case that matters is [16]byte: it is
// how a UUID arrives when a model does not hold it as a string, and
// fmt.Sprint would render it as "[15 140 75 ...]", which a policy casting to
// uuid rejects — a failure that reads as a broken policy rather than a
// mis-rendered value.
func tenantSettingText(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case [16]byte:
		return fmt.Sprintf("%x-%x-%x-%x-%x",
			typed[0:4], typed[4:6], typed[6:8], typed[8:10], typed[10:16]), nil
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return fmt.Sprint(typed), nil
	}
	// Deliberately not fmt.Sprint as a fallback. A type the tenant seam does
	// not recognise is a binding nobody can predict the database's reading
	// of, and metadata.go already refuses such a type on the model — so
	// reaching here means a value was bound that no model could carry.
	return "", fmt.Errorf("%w: cannot render a tenant of type %T as a setting",
		ErrTenantSettingUnsupported, value)
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

	tx, err := db.beginTx(ctx, opts)
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

// BindTenant binds an externally opened transaction after the application has
// created or resolved the tenant inside it. The configured tenant-enter hook
// runs before tenant-scoped ORM queries are allowed to execute.
func (tx *Tx) BindTenant(ctx context.Context, value any) error {
	if tx.closed {
		return ErrClosed
	}
	if value == nil {
		return fmt.Errorf("%w: BindTenant needs a tenant", ErrNoTenant)
	}
	if tx.tenant != nil {
		return fmt.Errorf("orm: transaction is already bound to a tenant")
	}
	if tx.db.tenantEnter == nil {
		return fmt.Errorf("%w: no tenant-enter hook is configured", ErrTenantSettingUnsupported)
	}
	if err := tx.db.tenantEnter(ctx, tx, value); err != nil {
		return err
	}
	tx.tenant = &TenantBinding{Value: value}
	return nil
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
