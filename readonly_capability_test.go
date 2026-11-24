package orm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// A dialect that cannot open a read-only transaction has the option dropped
// rather than the call refused.
//
// # Why a fake driver and not a real SQLite file
//
// Because the behavioural version of this test cannot be written. ReadOnly is
// honoured by the DRIVER, and mattn/go-sqlite3 — the driver this package's own
// tests use — silently ignores it: a write inside a read-only transaction
// succeeds. (Which also means orm/tenant.go's promise that ReadOnly "turns 'a
// read path wrote something' into a database error" is already untrue on
// SQLite with that driver. Noted here because it is the kind of thing that
// reads as working.)
//
// So the assertion is the precise one: what driver.TxOptions actually reached
// the driver.
//
// Measured against libSQL's sqld 0.24.33, whose BeginTx answers "read only
// transactions are not supported" — which would otherwise turn every read
// request in lemmego/tenancy into a 500, since tenancy/provider passes
// &sql.TxOptions{ReadOnly: true} on every read path.

var txRecorderSequence atomic.Int64

type txRecorderDriver struct{ conn *txRecorderConn }

func (d *txRecorderDriver) Open(string) (driver.Conn, error) { return d.conn, nil }

type txRecorderConn struct {
	begins   int
	lastOpts driver.TxOptions
}

func (c *txRecorderConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("recorder: prepare is not supported")
}
func (c *txRecorderConn) Close() error { return nil }
func (c *txRecorderConn) Begin() (driver.Tx, error) {
	return nil, errors.New("recorder: use BeginTx")
}

// ConnBeginTx is the interface database/sql requires before it will pass
// ReadOnly or an isolation level through at all — without it, BeginTx with
// ReadOnly fails inside database/sql and never reaches a driver.
func (c *txRecorderConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.begins++
	c.lastOpts = opts
	return txRecorderTx{}, nil
}

type txRecorderTx struct{}

func (txRecorderTx) Commit() error   { return nil }
func (txRecorderTx) Rollback() error { return nil }

var _ driver.ConnBeginTx = (*txRecorderConn)(nil)

func openRecorder(t *testing.T, dialect Dialect) (*DB, *txRecorderConn) {
	t.Helper()
	conn := &txRecorderConn{}
	name := fmt.Sprintf("orm_txrecorder_%d", txRecorderSequence.Add(1))
	sql.Register(name, &txRecorderDriver{conn: conn})

	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	return Open(sqlDB, dialect), conn
}

func TestAReadOnlyTransactionIsDowngradedOnlyForADialectThatCannotDoIt(t *testing.T) {
	ctx := context.Background()

	for _, c := range []struct {
		name         string
		dialect      Dialect
		wantReadOnly bool
	}{
		// The control. If this ever flips, the downgrade has leaked into a
		// dialect that never asked for it — which would silently remove the
		// assertion from Postgres, where it does work.
		{"SQLite keeps it", SQLite(), true},
		{"Postgres keeps it", Postgres(), true},
		{"LibSQL loses it", LibSQL(), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, conn := openRecorder(t, c.dialect)

			tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			defer func() { _ = tx.Rollback() }()

			if conn.begins != 1 {
				t.Fatalf("driver saw %d BeginTx calls, want 1", conn.begins)
			}
			if conn.lastOpts.ReadOnly != c.wantReadOnly {
				t.Errorf("driver received ReadOnly=%v, want %v",
					conn.lastOpts.ReadOnly, c.wantReadOnly)
			}
		})
	}
}

// The caller's struct must not be mutated. tenancy hands the same intent in on
// every request, and a downgrade that wrote through the pointer would turn one
// unsupported backend into a process-wide loss of the option.
func TestTheDowngradeDoesNotMutateTheCallersOptions(t *testing.T) {
	db, _ := openRecorder(t, LibSQL())

	opts := &sql.TxOptions{ReadOnly: true}
	tx, err := db.BeginTx(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()

	if !opts.ReadOnly {
		t.Error("the caller's TxOptions was mutated")
	}
}

// The capability is declared by implementing and read by type assertion, so a
// dialect that says nothing is capable. Pinned because the default direction
// matters: defaulting to "cannot" would strip the option from Postgres.
func TestOnlyADialectThatSaysSoLosesReadOnlyTransactions(t *testing.T) {
	for _, c := range []struct {
		dialect Dialect
		want    bool
	}{
		{Postgres(), true},
		{MySQL(), true},
		{SQLite(), true},
		{LibSQL(), false},
	} {
		if got := SupportsReadOnlyTransactions(c.dialect); got != c.want {
			t.Errorf("SupportsReadOnlyTransactions(%s) = %v, want %v", c.dialect.Name(), got, c.want)
		}
	}
}

// LibSQL is SQLite's syntax with one capability removed. Embedding provides
// that; a named test keeps somebody from "simplifying" it into an alias, which
// would make every switch on the dialect treat it as SQLite and lose the
// capability.
func TestLibSQLKeepsSQLitesSyntaxButNotItsName(t *testing.T) {
	libsql, sqlite := LibSQL(), SQLite()

	if libsql.Name() != "libsql" {
		t.Errorf("Name() = %q, want libsql — errors should name the real backend", libsql.Name())
	}
	if libsql.Placeholder(1) != sqlite.Placeholder(1) {
		t.Error("placeholders differ from SQLite")
	}
	if libsql.SupportsReturning() != sqlite.SupportsReturning() {
		t.Error("RETURNING support differs from SQLite")
	}
	if libsql.Quote("x") != sqlite.Quote("x") {
		t.Error("quoting differs from SQLite")
	}
	got, err := libsql.UpsertClause([]string{"a"}, []string{"b"})
	want, _ := sqlite.UpsertClause([]string{"a"}, []string{"b"})
	if err != nil || got != want {
		t.Errorf("upsert clause = %q (err %v), want %q", got, err, want)
	}
}
