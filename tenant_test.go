package orm

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"
)

// The tenant seam, tested from the outside: can one tenant reach another
// tenant's rows through any path this package offers?

const tenantSchema = `
CREATE TABLE docs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id INTEGER NOT NULL,
    title        TEXT,
    deleted_at   DATETIME
);
CREATE TABLE doc_notes (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id INTEGER NOT NULL,
    doc_id       INTEGER,
    body         TEXT
);
CREATE TABLE memos (
    id    INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT
);
`

type Doc struct {
	WorkspaceID uint64     `db:"workspace_id" orm:"column:workspace_id;tenantKey"`
	ID          uint64     `db:"id" orm:"primaryKey;autoIncrement"`
	Title       string     `db:"title"`
	DeletedAt   *time.Time `db:"deleted_at"`
	Notes       []DocNote  `orm:"hasMany:DocID"`
}

type DocNote struct {
	WorkspaceID uint64 `db:"workspace_id" orm:"column:workspace_id;tenantKey"`
	ID          uint64 `db:"id" orm:"primaryKey;autoIncrement"`
	DocID       uint64 `db:"doc_id"`
	Body        string `db:"body"`
}

// Memo is not tenant-scoped, and is here to prove the seam is inert for a
// model that does not ask for it.
type Memo struct {
	ID    uint64 `db:"id" orm:"primaryKey;autoIncrement"`
	Title string `db:"title"`
}

const (
	alpha uint64 = 1
	beta  uint64 = 2
)

// Note for anyone extending these tests: never call t.Fatal inside a
// TenantTx callback. t.Fatal exits the goroutine, so the callback never
// returns, the transaction is neither committed nor rolled back, and with the
// pool capped at one connection the next statement blocks forever. Capture
// what you need and assert after the transaction has closed.
func openTenantDB(t *testing.T) (*DB, context.Context) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	if _, err := sqlDB.Exec(tenantSchema); err != nil {
		t.Fatal(err)
	}
	return Open(sqlDB, SQLite()), context.Background()
}

// seedTwoTenants writes one doc and one note per tenant, through the ORM, so
// the fixture itself exercises the stamping path.
func seedTwoTenants(t *testing.T, db *DB, ctx context.Context) {
	t.Helper()
	for _, ws := range []uint64{alpha, beta} {
		if err := db.TenantTx(ctx, ws, func(tx *Tx) error {
			doc := &Doc{Title: "doc of " + itoa(ws)}
			if _, err := tx.Model[Doc]().Create(ctx, doc); err != nil {
				return err
			}
			note := &DocNote{DocID: doc.ID, Body: "note of " + itoa(ws)}
			_, err := tx.Model[DocNote]().Create(ctx, note)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func itoa(n uint64) string { return string(rune('0' + n)) }

func TestAnUnboundHandleCannotQueryATenantScopedModel(t *testing.T) {
	db, ctx := openTenantDB(t)

	// Every terminal method must report it, not just the one that happens to
	// be tested: a construction-time failure that only surfaced from All
	// would let Count or Create run unfiltered.
	t.Run("All", func(t *testing.T) {
		_, err := db.Model[Doc]().All(ctx)
		requireErrNoTenant(t, err)
	})
	t.Run("First", func(t *testing.T) {
		_, err := db.Model[Doc]().First(ctx)
		requireErrNoTenant(t, err)
	})
	t.Run("Count", func(t *testing.T) {
		_, err := db.Model[Doc]().Count(ctx)
		requireErrNoTenant(t, err)
	})
	t.Run("Create", func(t *testing.T) {
		_, err := db.Model[Doc]().Create(ctx, &Doc{Title: "x"})
		requireErrNoTenant(t, err)
	})
	t.Run("an ordinary transaction is just as unbound", func(t *testing.T) {
		err := db.Transaction(ctx, func(tx *Tx) error {
			_, err := tx.Model[Doc]().All(ctx)
			return err
		})
		requireErrNoTenant(t, err)
	})
}

func requireErrNoTenant(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrNoTenant) {
		t.Fatalf("want ErrNoTenant, got %v", err)
	}
}

func TestAModelWithNoTenantKeyIsUnaffected(t *testing.T) {
	db, ctx := openTenantDB(t)
	if _, err := db.Model[Memo]().Create(ctx, &Memo{Title: "untenanted"}); err != nil {
		t.Fatalf("an untenanted model must still work on an unbound handle: %v", err)
	}
	memos, err := db.Model[Memo]().All(ctx)
	if err != nil || len(memos) != 1 {
		t.Fatalf("got %d memos, err %v", len(memos), err)
	}
}

func TestReadsAreFilteredToTheBoundTenant(t *testing.T) {
	db, ctx := openTenantDB(t)
	seedTwoTenants(t, db, ctx)

	for _, ws := range []uint64{alpha, beta} {
		var seen []Doc
		if err := db.TenantTx(ctx, ws, func(tx *Tx) error {
			docs, err := tx.Model[Doc]().All(ctx)
			seen = docs
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(seen) != 1 {
			t.Fatalf("tenant %d saw %d docs, want 1", ws, len(seen))
		}
		if seen[0].WorkspaceID != ws {
			t.Fatalf("tenant %d saw a doc belonging to %d", ws, seen[0].WorkspaceID)
		}
	}
}

func TestAnInsertIsStampedWithTheBoundTenant(t *testing.T) {
	db, ctx := openTenantDB(t)

	// The caller never sets WorkspaceID. That is the point: a handler cannot
	// forget to do something it never does.
	doc := &Doc{Title: "stamped"}
	if err := db.TenantTx(ctx, alpha, func(tx *Tx) error {
		_, err := tx.Model[Doc]().Create(ctx, doc)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if doc.WorkspaceID != alpha {
		t.Fatalf("the entity should carry the tenant it was written with, got %d", doc.WorkspaceID)
	}

	var stored uint64
	if err := db.SQLDB().QueryRow(`SELECT workspace_id FROM docs WHERE id = ?`, doc.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != alpha {
		t.Fatalf("stored workspace_id = %d, want %d", stored, alpha)
	}
}

func TestAnInsertCarryingAnotherTenantIsRefused(t *testing.T) {
	db, ctx := openTenantDB(t)
	err := db.TenantTx(ctx, alpha, func(tx *Tx) error {
		_, err := tx.Model[Doc]().Create(ctx, &Doc{WorkspaceID: beta, Title: "smuggled"})
		return err
	})
	if !errors.Is(err, ErrTenantMismatch) {
		t.Fatalf("want ErrTenantMismatch, got %v", err)
	}
}

func TestABatchInsertIsStampedToo(t *testing.T) {
	db, ctx := openTenantDB(t)
	if err := db.TenantTx(ctx, alpha, func(tx *Tx) error {
		_, err := tx.Model[Doc]().CreateBatch(ctx, []*Doc{{Title: "a"}, {Title: "b"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.SQLDB().QueryRow(`SELECT count(*) FROM docs WHERE workspace_id = ?`, alpha).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("got %d rows stamped for alpha, want 2", n)
	}
}

// The write paths that address one row by its primary key do NOT go through
// scopedPredicates, so they are the ones most likely to be missed.
func TestSingleRowWritesCannotCrossTenants(t *testing.T) {
	db, ctx := openTenantDB(t)
	seedTwoTenants(t, db, ctx)

	// Beta's doc, as beta sees it.
	var betaDoc Doc
	if err := db.TenantTx(ctx, beta, func(tx *Tx) error {
		found, err := tx.Model[Doc]().First(ctx)
		if err == nil {
			betaDoc = *found
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("Update", func(t *testing.T) {
		victim := betaDoc
		victim.Title = "rewritten by alpha"
		var updateErr error
		if err := db.TenantTx(ctx, alpha, func(tx *Tx) error {
			// The entity names beta, so the mismatch is caught before SQL.
			_, updateErr = tx.Model[Doc]().Update(ctx, &victim)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(updateErr, ErrTenantMismatch) {
			t.Fatalf("want ErrTenantMismatch, got %v", updateErr)
		}
		assertTitleUnchanged(t, db, betaDoc.ID, betaDoc.Title)
	})

	t.Run("Update with the tenant column cleared", func(t *testing.T) {
		// The subtler attempt: strip the tenant so the mismatch check passes
		// and the stamp fills in alpha. The WHERE must still miss, because
		// the row in the database belongs to beta.
		victim := betaDoc
		victim.WorkspaceID = 0
		victim.Title = "rewritten by alpha"
		var affected int64
		if err := db.TenantTx(ctx, alpha, func(tx *Tx) error {
			result, err := tx.Model[Doc]().Update(ctx, &victim)
			if err != nil {
				return err
			}
			affected, _ = result.RowsAffected()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if affected != 0 {
			t.Fatalf("alpha updated %d of beta's rows, want 0", affected)
		}
		assertTitleUnchanged(t, db, betaDoc.ID, betaDoc.Title)
	})

	t.Run("Delete", func(t *testing.T) {
		victim := betaDoc
		victim.WorkspaceID = 0
		if err := db.TenantTx(ctx, alpha, func(tx *Tx) error {
			if _, err := tx.Model[Doc]().Delete(ctx, &victim); err != nil {
				return err
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		// A soft delete, so the row survives with deleted_at NULL.
		var deletedAt sql.NullTime
		if err := db.SQLDB().QueryRow(`SELECT deleted_at FROM docs WHERE id = ?`, betaDoc.ID).Scan(&deletedAt); err != nil {
			t.Fatal(err)
		}
		if deletedAt.Valid {
			t.Fatal("alpha soft-deleted beta's row")
		}
	})
}

func assertTitleUnchanged(t *testing.T, db *DB, id uint64, want string) {
	t.Helper()
	var title string
	if err := db.SQLDB().QueryRow(`SELECT title FROM docs WHERE id = ?`, id).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != want {
		t.Fatalf("the row was modified across tenants: title = %q, want %q", title, want)
	}
}

// The implicit predicates must stay out of the caller's set, or a tenant
// filter would silently satisfy the guard that stops an unqualified UPDATE
// rewriting a whole table.
func TestTheBulkMutationGuardStillFiresUnderATenantFilter(t *testing.T) {
	db, ctx := openTenantDB(t)
	seedTwoTenants(t, db, ctx)

	err := db.TenantTx(ctx, alpha, func(tx *Tx) error {
		_, err := tx.Model[Doc]().UpdateAll(ctx, map[string]any{"title": "all of them"})
		return err
	})
	if !errors.Is(err, ErrUnsafeMutation) {
		t.Fatalf("want ErrUnsafeMutation, got %v", err)
	}

	// And with a real caller predicate it proceeds — scoped to alpha only.
	if err := db.TenantTx(ctx, alpha, func(tx *Tx) error {
		_, err := tx.Model[Doc]().Where(Gt("id", 0)).UpdateAll(ctx, map[string]any{"title": "alpha only"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.SQLDB().QueryRow(
		`SELECT count(*) FROM docs WHERE title = 'alpha only' AND workspace_id != ?`, alpha).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a bulk update reached %d rows outside the tenant", n)
	}
}

func TestAsKeepsTheTenantBinding(t *testing.T) {
	db, ctx := openTenantDB(t)
	seedTwoTenants(t, db, ctx)

	type titleOnly struct {
		Title string `db:"title"`
	}
	var rows []titleOnly
	if err := db.TenantTx(ctx, alpha, func(tx *Tx) error {
		found, err := tx.Model[Doc]().As[titleOnly]().All(ctx)
		rows = found
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("a reprojection saw %d rows, want 1 — the binding was dropped by As", len(rows))
	}
}

func TestEagerLoadingIsFilteredToTheBoundTenant(t *testing.T) {
	db, ctx := openTenantDB(t)
	seedTwoTenants(t, db, ctx)

	// Give alpha's doc a note that belongs to beta, which is the shape a
	// cross-tenant leak through a relation would take.
	if _, err := db.SQLDB().Exec(
		`INSERT INTO doc_notes (workspace_id, doc_id, body) SELECT ?, id, 'planted' FROM docs WHERE workspace_id = ?`,
		beta, alpha); err != nil {
		t.Fatal(err)
	}

	var docs []Doc
	if err := db.TenantTx(ctx, alpha, func(tx *Tx) error {
		found, err := tx.Model[Doc]().With("Notes").All(ctx)
		docs = found
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}
	for _, note := range docs[0].Notes {
		if note.WorkspaceID != alpha {
			t.Fatalf("an eager load returned a note belonging to %d", note.WorkspaceID)
		}
	}
}

func TestTheTenantEnterHookRunsBeforeTheFirstStatement(t *testing.T) {
	sqlDB, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()
	if _, err := sqlDB.Exec(tenantSchema); err != nil {
		t.Fatal(err)
	}

	var order []string
	var toldValue any
	db := Open(sqlDB, SQLite(), WithTenantEnter(func(ctx context.Context, tx *Tx, value any) error {
		order = append(order, "enter")
		toldValue = value
		return nil
	}))

	ctx := context.Background()
	var reported any
	var reportedOK bool
	if err := db.TenantTx(ctx, alpha, func(tx *Tx) error {
		order = append(order, "body")
		reported, reportedOK = tx.TenantValue()
		_, err := tx.Model[Doc]().All(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if toldValue != alpha {
		t.Fatalf("the hook was told %v, want %v", toldValue, alpha)
	}
	if !reportedOK || reported != alpha {
		t.Fatalf("TenantValue = %v, %v", reported, reportedOK)
	}
	if len(order) != 2 || order[0] != "enter" || order[1] != "body" {
		t.Fatalf("the database must be told the tenant before anything runs: %v", order)
	}
}

func TestAFailingTenantEnterHookAbortsTheTransaction(t *testing.T) {
	sqlDB, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()
	if _, err := sqlDB.Exec(tenantSchema); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("could not scope the session")
	db := Open(sqlDB, SQLite(), WithTenantEnter(func(context.Context, *Tx, any) error { return boom }))

	ran := false
	err = db.TenantTx(context.Background(), alpha, func(*Tx) error { ran = true; return nil })
	if !errors.Is(err, boom) {
		t.Fatalf("want the hook's error, got %v", err)
	}
	if ran {
		t.Fatal("the body ran even though the database was never scoped")
	}
}

func TestTenantTxNeedsATenant(t *testing.T) {
	db, ctx := openTenantDB(t)
	err := db.TenantTx(ctx, nil, func(*Tx) error { return nil })
	if !errors.Is(err, ErrNoTenant) {
		t.Fatalf("want ErrNoTenant, got %v", err)
	}
}

// A tenant key that cannot hold a tenant is refused when the model is parsed,
// rather than producing rows no predicate and no policy can match.
func TestAnUnusableTenantKeyIsRefusedAtParseTime(t *testing.T) {
	type nullableKey struct {
		WorkspaceID *uint64 `db:"workspace_id" orm:"tenantKey"`
		ID          uint64  `db:"id" orm:"primaryKey"`
	}
	type timeKey struct {
		WorkspaceID time.Time `db:"workspace_id" orm:"tenantKey"`
		ID          uint64    `db:"id" orm:"primaryKey"`
	}

	for name, fn := range map[string]func() error{
		"a pointer": func() error { _, err := modelInfoFor(reflect.TypeFor[nullableKey]()); return err },
		"a time":    func() error { _, err := modelInfoFor(reflect.TypeFor[timeKey]()); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(); err == nil {
				t.Fatal("want a parse error for an unusable tenant key, got nil")
			}
		})
	}
}
