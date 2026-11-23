package orm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The RETURNING path cannot be exercised by SQLite, so a stub driver stands in
// for a database that supports it. This asserts the statement that is actually
// sent and that the generated key is read back off the returned row.

var fakeDriverSequence atomic.Int64

type fakeDriver struct{ conn *fakeConn }

func (d *fakeDriver) Open(string) (driver.Conn, error) { return d.conn, nil }

type fakeConn struct {
	columns    []string
	rows       [][]driver.Value
	lastInsert int64

	lastQuery  string
	queryCalls int
	execCalls  int
	lastArgs   []driver.NamedValue
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake: prepare is not supported")
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return nil, errors.New("fake: no transactions") }

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.queryCalls++
	c.lastQuery = query
	c.lastArgs = append([]driver.NamedValue(nil), args...)
	return &fakeRows{columns: c.columns, data: c.rows}, nil
}

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.execCalls++
	c.lastQuery = query
	c.lastArgs = append([]driver.NamedValue(nil), args...)
	return fakeExecResult{lastInsert: c.lastInsert}, nil
}

type fakeExecResult struct{ lastInsert int64 }

func (r fakeExecResult) LastInsertId() (int64, error) { return r.lastInsert, nil }
func (r fakeExecResult) RowsAffected() (int64, error) { return 1, nil }

type fakeRows struct {
	columns []string
	data    [][]driver.Value
	pos     int
}

func (r *fakeRows) Columns() []string { return r.columns }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

func openFakeDB(t *testing.T, dialect Dialect, conn *fakeConn) *DB {
	t.Helper()
	name := fmt.Sprintf("orm_fake_%d", fakeDriverSequence.Add(1))
	sql.Register(name, &fakeDriver{conn: conn})

	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	return Open(sqlDB, dialect)
}

func TestPostgresInsertUsesReturning(t *testing.T) {
	conn := &fakeConn{columns: []string{"id"}, rows: [][]driver.Value{{int64(42)}}}
	db := openFakeDB(t, Postgres(), conn)

	user := &User{Email: "returning@example.com", CreatedAt: 1}
	result, err := db.Model[User]().Create(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasSuffix(conn.lastQuery, ` RETURNING "id"`) {
		t.Fatalf("expected a RETURNING clause, got: %s", conn.lastQuery)
	}
	if conn.queryCalls != 1 || conn.execCalls != 0 {
		t.Fatalf("RETURNING must go through a query, not an exec: %d queries, %d execs", conn.queryCalls, conn.execCalls)
	}
	if user.ID != 42 {
		t.Fatalf("expected the returned key to be written back, got %d", user.ID)
	}

	// LastInsertId is meaningless on this path and must say so rather than
	// silently reporting zero.
	if _, err := result.LastInsertId(); err == nil {
		t.Fatal("expected LastInsertId to report that it is unavailable")
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("unexpected rows affected: %d %v", rows, err)
	}
}

func TestMySQLInsertUsesLastInsertId(t *testing.T) {
	conn := &fakeConn{lastInsert: 7}
	db := openFakeDB(t, MySQL(), conn)

	user := &User{Email: "lastid@example.com", CreatedAt: 1}
	if _, err := db.Model[User]().Create(context.Background(), user); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(conn.lastQuery, "RETURNING") {
		t.Fatalf("MySQL must not emit RETURNING: %s", conn.lastQuery)
	}
	if conn.execCalls != 1 || conn.queryCalls != 0 {
		t.Fatalf("expected a single exec: %d execs, %d queries", conn.execCalls, conn.queryCalls)
	}
	if user.ID != 7 {
		t.Fatalf("expected LastInsertId to populate the key, got %d", user.ID)
	}
}

// A caller-supplied key must be preserved, not replaced by a generated one.
func TestExplicitKeySkipsGeneration(t *testing.T) {
	conn := &fakeConn{lastInsert: 99}
	db := openFakeDB(t, MySQL(), conn)

	user := &User{ID: 5, Email: "explicit@example.com"}
	if _, err := db.Model[User]().Create(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	if user.ID != 5 {
		t.Fatalf("an explicit key must survive insert, got %d", user.ID)
	}
	if !strings.Contains(conn.lastQuery, "`id`") {
		t.Fatalf("an explicit key must be written as a column: %s", conn.lastQuery)
	}
}

func TestDefaultPrimaryKeyUsesReturning(t *testing.T) {
	conn := &fakeConn{columns: []string{"id"}, rows: [][]driver.Value{{"generated-uuid"}}}
	db := openFakeDB(t, Postgres(), conn)

	entity := &GeneratedUUID{Name: "entry"}
	if _, err := db.Model[GeneratedUUID]().Create(context.Background(), entity); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conn.lastQuery, `INSERT INTO "generated_uuids" ("name") VALUES ($1) RETURNING "id"`) {
		t.Fatalf("expected generated default key to be omitted and returned, got %s", conn.lastQuery)
	}
	if entity.ID != "generated-uuid" {
		t.Fatalf("expected generated key to be written back, got %q", entity.ID)
	}
}

type GuardedEntity struct {
	ID     int    `orm:"primaryKey;autoIncrement"`
	SiteID int    `db:"site_id"`
	Title  string `db:"title"`
}

type ClientSiteGrant struct {
	ID     int `orm:"primaryKey;autoIncrement"`
	SiteID int `db:"site_id"`
	UserID int `db:"user_id"`
}

func TestCreateIfKeepsGuardAndInsertAtomic(t *testing.T) {
	conn := &fakeConn{columns: []string{"id"}, rows: [][]driver.Value{{int64(5)}}}
	db := openFakeDB(t, Postgres(), conn)
	entity := &GuardedEntity{SiteID: 12, Title: "report"}
	guard := db.Model[ClientSiteGrant]().Where(Eq("site_id", 12), Eq("user_id", 9))
	if _, err := CreateIf(context.Background(), db.Model[GuardedEntity](), entity, guard); err != nil {
		t.Fatal(err)
	}
	want := `INSERT INTO "guarded_entities" ("site_id", "title") SELECT $1, $2 WHERE EXISTS (SELECT 1 FROM "client_site_grants" WHERE "site_id" = $3 AND "user_id" = $4) RETURNING "id"`
	if conn.lastQuery != want {
		t.Fatalf("got %s, want %s", conn.lastQuery, want)
	}
	if len(conn.lastArgs) != 4 || conn.lastArgs[0].Value != int64(12) || conn.lastArgs[1].Value != "report" || conn.lastArgs[2].Value != int64(12) || conn.lastArgs[3].Value != int64(9) {
		t.Fatalf("unexpected bound arguments: %#v", conn.lastArgs)
	}
	if entity.ID != 5 {
		t.Fatalf("expected generated ID 5, got %d", entity.ID)
	}
}

func TestUpsertExprRenumbersBoundAssignmentExpressions(t *testing.T) {
	conn := &fakeConn{}
	db := openFakeDB(t, Postgres(), conn)
	user := &User{Email: "upsert@example.com", CreatedAt: 17}
	_, err := db.Model[User]().UpsertExpr(context.Background(), user, []string{"email_address"}, map[string]SQLExpr{
		"name": Expr("CASE WHEN users.created_at = ? THEN ? ELSE ? END", int64(17), "same", "changed"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `INSERT INTO "users" ("email_address", "created_at") VALUES ($1, $2) ON CONFLICT ("email_address") DO UPDATE SET "name" = CASE WHEN users.created_at = $3 THEN $4 ELSE $5 END`
	if conn.lastQuery != want {
		t.Fatalf("got %s, want %s", conn.lastQuery, want)
	}
	if len(conn.lastArgs) != 5 || conn.lastArgs[2].Value != int64(17) || conn.lastArgs[3].Value != "same" || conn.lastArgs[4].Value != "changed" {
		t.Fatalf("unexpected arguments: %#v", conn.lastArgs)
	}
}

func TestUpsertExprWhereKeepsMonotonicPredicateAtomic(t *testing.T) {
	conn := &fakeConn{}
	db := openFakeDB(t, Postgres(), conn)
	user := &User{Email: "upsert-where@example.com", CreatedAt: 17}
	_, err := db.Model[User]().UpsertExprWhere(context.Background(), user, []string{"email_address"}, map[string]SQLExpr{
		"name": Expr("EXCLUDED.name"),
	}, ColGte("excluded.created_at", "users.created_at"))
	if err != nil {
		t.Fatal(err)
	}
	want := `INSERT INTO "users" ("email_address", "created_at") VALUES ($1, $2) ON CONFLICT ("email_address") DO UPDATE SET "name" = EXCLUDED.name WHERE "excluded"."created_at" >= "users"."created_at"`
	if conn.lastQuery != want {
		t.Fatalf("got %s, want %s", conn.lastQuery, want)
	}
}

func TestInsertIgnoreReturningDistinguishesInsertedFromConflict(t *testing.T) {
	for _, tt := range []struct {
		name string
		rows [][]driver.Value
		want bool
	}{
		{name: "inserted", rows: [][]driver.Value{{"token-id"}}, want: true},
		{name: "conflict", rows: nil, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn := &fakeConn{columns: []string{"id"}, rows: tt.rows}
			db := openFakeDB(t, Postgres(), conn)
			inserted, err := db.Model[HandoffToken]().InsertIgnoreReturning(context.Background(), &HandoffToken{ID: "token-id", ExpiresAt: time.Now()}, "id")
			if err != nil {
				t.Fatal(err)
			}
			if inserted != tt.want {
				t.Fatalf("inserted = %v, want %v", inserted, tt.want)
			}
			if !strings.Contains(conn.lastQuery, `RETURNING "id"`) {
				t.Fatalf("expected RETURNING clause, got %s", conn.lastQuery)
			}
		})
	}
}

type HandoffToken struct {
	ID         string    `orm:"primaryKey"`
	ExpiresAt  time.Time `db:"expires_at"`
	ConsumedAt time.Time `db:"consumed_at" orm:"default"`
}
