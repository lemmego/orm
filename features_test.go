package orm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- query logging --------------------------------------------------------

type recorder struct {
	mu     sync.Mutex
	events []QueryEvent
}

func (r *recorder) log() Logger {
	return func(_ context.Context, event QueryEvent) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, event)
	}
}

func (r *recorder) all() []QueryEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]QueryEvent(nil), r.events...)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

func openLoggedDB(t *testing.T) (*DB, *recorder) {
	t.Helper()
	log := &recorder{}
	db := openTestDB(t)
	logged := Open(db.SQLDB(), SQLite(), WithLogger(log.log()))
	return logged, log
}

func TestLoggerSeesEveryStatement(t *testing.T) {
	db, log := openLoggedDB(t)
	ctx := context.Background()

	user := &User{Email: "logged@example.com"}
	if _, err := db.Model[User]().Create(ctx, user); err != nil {
		t.Fatal(err)
	}
	events := log.all()
	if len(events) != 1 {
		t.Fatalf("expected one statement, got %d", len(events))
	}
	if events[0].Operation != OpExec {
		t.Fatalf("an insert is an exec, got %s", events[0].Operation)
	}
	if !strings.Contains(events[0].SQL, "INSERT INTO") {
		t.Fatalf("unexpected SQL: %s", events[0].SQL)
	}
	if len(events[0].Args) == 0 {
		t.Fatal("expected the bound arguments to be reported")
	}
	if events[0].Rows != 1 {
		t.Fatalf("expected 1 row affected, got %d", events[0].Rows)
	}
	if events[0].Duration <= 0 {
		t.Fatal("expected a measured duration")
	}

	log.reset()
	if _, err := db.Model[User]().All(ctx); err != nil {
		t.Fatal(err)
	}
	events = log.all()
	if len(events) != 1 || events[0].Operation != OpQuery {
		t.Fatalf("expected one query event, got %#v", events)
	}
	// Row counts are unknowable for a read at statement time.
	if events[0].Rows != -1 {
		t.Fatalf("expected an unknown row count for a query, got %d", events[0].Rows)
	}
}

// Eager loading issues extra statements the caller never wrote; the logger has
// to see those too, since they are exactly what one wants to inspect.
func TestLoggerSeesEagerLoadingQueries(t *testing.T) {
	db, log := openLoggedDB(t)
	ctx := context.Background()

	user := &User{Email: "parent@example.com"}
	if _, err := db.Model[User]().Create(ctx, user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[Post]().Create(ctx, &Post{UserID: user.ID, Title: "child"}); err != nil {
		t.Fatal(err)
	}

	log.reset()
	if _, err := db.Model[User]().With("Posts").All(ctx); err != nil {
		t.Fatal(err)
	}
	events := log.all()
	if len(events) != 2 {
		t.Fatalf("expected the root query and the relation query, got %d", len(events))
	}
	if !strings.Contains(events[1].SQL, "posts") {
		t.Fatalf("the relation query was not logged: %s", events[1].SQL)
	}
}

func TestLoggerReportsFailures(t *testing.T) {
	db, log := openLoggedDB(t)

	_, err := db.Exec(context.Background(), "SELECT * FROM nope")
	if err == nil {
		t.Fatal("expected the statement to fail")
	}
	events := log.all()
	if len(events) != 1 || events[0].Err == nil {
		t.Fatalf("the failure should be reported to the logger: %#v", events)
	}
}

func TestQueryEventString(t *testing.T) {
	event := QueryEvent{
		Operation: OpQuery,
		SQL:       "SELECT  *\n FROM users",
		Args:      []any{1},
		Rows:      -1,
	}
	rendered := event.String()
	if !strings.Contains(rendered, "SELECT * FROM users") {
		t.Fatalf("whitespace should be collapsed: %q", rendered)
	}
	if strings.Contains(rendered, "rows=") {
		t.Fatalf("an unknown row count should be omitted: %q", rendered)
	}
}

// An ORM without a logger must not pay for one.
func TestNoLoggerLeavesExecutorUnwrapped(t *testing.T) {
	db := openTestDB(t)
	if db.Logger() != nil {
		t.Fatal("expected no logger by default")
	}
	if _, ok := db.executor().(*loggingExecutor); ok {
		t.Fatal("the executor must not be wrapped when no logger is installed")
	}
}

// --- upsert ---------------------------------------------------------------

func TestUpsertSQLPerDialect(t *testing.T) {
	cases := []struct {
		dialect Dialect
		update  []string
		want    string
	}{
		{Postgres(), []string{"email_address"}, `ON CONFLICT ("id") DO UPDATE SET "email_address" = EXCLUDED."email_address"`},
		{SQLite(), []string{"email_address"}, `ON CONFLICT ("id") DO UPDATE SET "email_address" = EXCLUDED."email_address"`},
		{MySQL(), []string{"email_address"}, "ON DUPLICATE KEY UPDATE `email_address` = VALUES(`email_address`)"},
		{Postgres(), nil, `ON CONFLICT ("id") DO NOTHING`},
		{MySQL(), nil, "ON DUPLICATE KEY UPDATE `id` = `id`"},
	}
	for _, testCase := range cases {
		got, err := testCase.dialect.UpsertClause([]string{"id"}, testCase.update)
		if err != nil {
			t.Fatal(err)
		}
		if got != testCase.want {
			t.Fatalf("%s:\n got  %s\n want %s", testCase.dialect.Name(), got, testCase.want)
		}
	}

	// Updating on conflict needs a target under the standard spelling.
	if _, err := Postgres().UpsertClause(nil, []string{"email_address"}); err == nil {
		t.Fatal("expected postgres to require conflict columns for an update")
	}
}

func TestUpsertInsertsThenUpdates(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	if _, err := db.Exec(ctx, `CREATE UNIQUE INDEX users_email ON users (email_address)`); err != nil {
		t.Fatal(err)
	}

	first := &User{Email: "upsert@example.com", CreatedAt: 1}
	if _, err := db.Model[User]().Upsert(ctx, first, []string{"email_address"}, "created_at"); err != nil {
		t.Fatal(err)
	}
	second := &User{Email: "upsert@example.com", CreatedAt: 2}
	if _, err := db.Model[User]().Upsert(ctx, second, []string{"email_address"}, "created_at"); err != nil {
		t.Fatal(err)
	}

	count, err := db.Model[User]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("an upsert must not duplicate the row, found %d", count)
	}
	stored, err := db.Model[User]().Where(Eq("email_address", "upsert@example.com")).First(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CreatedAt != 2 {
		t.Fatalf("expected the row to be updated, got %d", stored.CreatedAt)
	}
}

func TestInsertIgnoreLeavesExistingRow(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	if _, err := db.Exec(ctx, `CREATE UNIQUE INDEX users_email ON users (email_address)`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Model[User]().Create(ctx, &User{Email: "ignore@example.com", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[User]().InsertIgnore(ctx, &User{Email: "ignore@example.com", CreatedAt: 99}, "email_address"); err != nil {
		t.Fatal(err)
	}

	stored, err := db.Model[User]().Where(Eq("email_address", "ignore@example.com")).First(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CreatedAt != 1 {
		t.Fatalf("InsertIgnore must leave the existing row alone, got %d", stored.CreatedAt)
	}
}

func TestUpsertRequiresConflictColumns(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	if _, err := db.Model[User]().Upsert(ctx, &User{}, nil); err == nil {
		t.Fatal("expected Upsert to require conflict columns")
	}
	if _, err := db.Model[User]().InsertIgnore(ctx, &User{}); err == nil {
		t.Fatal("expected InsertIgnore to require conflict columns")
	}
}

// The default update set is every mapped column except the conflict columns
// and the generated key.
func TestUpsertDefaultUpdateColumns(t *testing.T) {
	info := mustInfo[User](t)
	columns := updatableColumns(info, []string{"email_address"})
	if len(columns) != 1 || columns[0] != "created_at" {
		t.Fatalf("unexpected default update set: %#v", columns)
	}
}

// --- savepoints -----------------------------------------------------------

func TestTransactionSavepoints(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()

	if err := db.Transaction(ctx, func(tx *Tx) error {
		if _, err := tx.Model[User]().Create(ctx, &User{Email: "keep@example.com"}); err != nil {
			return err
		}
		if err := tx.Savepoint(ctx, "sp1"); err != nil {
			return err
		}
		if _, err := tx.Model[User]().Create(ctx, &User{Email: "undo@example.com"}); err != nil {
			return err
		}
		return tx.RollbackTo(ctx, "sp1")
	}); err != nil {
		t.Fatal(err)
	}

	users, err := db.Model[User]().All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Email != "keep@example.com" {
		t.Fatalf("expected only the pre-savepoint row: %#v", users)
	}
}

func TestReleaseSavepointKeepsWork(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()

	if err := db.Transaction(ctx, func(tx *Tx) error {
		if err := tx.Savepoint(ctx, "sp1"); err != nil {
			return err
		}
		if _, err := tx.Model[User]().Create(ctx, &User{Email: "kept@example.com"}); err != nil {
			return err
		}
		return tx.ReleaseSavepoint(ctx, "sp1")
	}); err != nil {
		t.Fatal(err)
	}
	count, err := db.Model[User]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("releasing a savepoint must keep the work, got %d rows", count)
	}
}

// Savepoint names are written into SQL, so they must be validated.
func TestSavepointNameIsValidated(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()

	err := db.Transaction(ctx, func(tx *Tx) error {
		return tx.Savepoint(ctx, "sp1; DROP TABLE users")
	})
	if !errors.Is(err, ErrInvalidIdentifier) {
		t.Fatalf("expected ErrInvalidIdentifier, got %v", err)
	}
	if _, err := db.Model[User]().Count(ctx); err != nil {
		t.Fatalf("the users table should still exist: %v", err)
	}

	for _, name := range []string{"", "1abc", "a-b", "a b", `a"b`} {
		if err := CheckIdentifier(name); err == nil {
			t.Fatalf("expected %q to be rejected", name)
		}
	}
	for _, name := range []string{"sp1", "_x", "a_b_9"} {
		if err := CheckIdentifier(name); err != nil {
			t.Fatalf("expected %q to be accepted: %v", name, err)
		}
	}
}

// A timestamp column whose declared type the driver does not recognise comes
// back as text. SQLite does this for "DATETIME(6)" — which is exactly what the
// migration builder emits — and for "TEXT", so a model using time.Time could
// not read its own rows back.
func TestTimestampsScanFromAnyDeclaredType(t *testing.T) {
	type Stamped struct {
		ID        int `orm:"primaryKey;autoIncrement"`
		CreatedAt time.Time
		DeletedAt *time.Time
		Noted     sql.NullTime
	}

	for _, declared := range []string{"DATETIME", "DATETIME(6)", "TIMESTAMP", "TEXT", "VARCHAR(64)"} {
		t.Run(declared, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			if _, err := db.Exec(ctx, `CREATE TABLE stampeds (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				created_at `+declared+`,
				deleted_at `+declared+`,
				noted `+declared+`)`); err != nil {
				t.Fatal(err)
			}

			when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
			row := &Stamped{CreatedAt: when, Noted: sql.NullTime{Time: when, Valid: true}}
			if _, err := db.Model[Stamped]().Create(ctx, row); err != nil {
				t.Fatalf("write: %v", err)
			}

			found, err := db.Model[Stamped]().Find(ctx, row.ID)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if !found.CreatedAt.Equal(when) {
				t.Fatalf("got %s, want %s", found.CreatedAt, when)
			}
			// A NULL timestamp must still land as the zero value.
			if found.DeletedAt != nil {
				t.Fatalf("expected nil for a NULL column, got %v", found.DeletedAt)
			}
			if !found.Noted.Valid || !found.Noted.Time.Equal(when) {
				t.Fatalf("NullTime did not round-trip: %#v", found.Noted)
			}
		})
	}
}

// Soft deletes rely on reading the marker back, so they must survive the same
// treatment.
func TestSoftDeleteWithTextTimestampColumn(t *testing.T) {
	type Doc struct {
		ID        int `orm:"primaryKey;autoIncrement"`
		Title     string
		DeletedAt *time.Time
	}
	db := openTestDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE docs (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT, deleted_at DATETIME(6))`); err != nil {
		t.Fatal(err)
	}

	doc := &Doc{Title: "draft"}
	if _, err := db.Model[Doc]().Create(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[Doc]().Delete(ctx, doc); err != nil {
		t.Fatal(err)
	}
	visible, err := db.Model[Doc]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("expected the row hidden, got %d", visible)
	}
	trashed, err := db.Model[Doc]().OnlyTrashed().All(ctx)
	if err != nil {
		t.Fatalf("reading trashed rows: %v", err)
	}
	if len(trashed) != 1 || trashed[0].DeletedAt == nil {
		t.Fatalf("expected one trashed row with a marker: %#v", trashed)
	}
}

// A soft-deleted row is hidden from direct queries, so it must be hidden from
// relations too. It previously still appeared through a parent's eager load,
// which meant "deleted" depended on how you reached the row.
func TestSoftDeletedChildrenAreNotEagerLoaded(t *testing.T) {
	type Chapter struct {
		ID        int `orm:"primaryKey;autoIncrement"`
		BookID    int
		Title     string
		DeletedAt *time.Time
	}
	type Book struct {
		ID       int `orm:"primaryKey;autoIncrement"`
		Name     string
		Chapters []Chapter `orm:"hasMany:BookID"`
	}

	db, ctx := openTestDB(t), context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE books (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT);
		CREATE TABLE chapters (id INTEGER PRIMARY KEY AUTOINCREMENT, book_id INTEGER, title TEXT, deleted_at DATETIME)`); err != nil {
		t.Fatal(err)
	}

	book := &Book{Name: "A Book"}
	if _, err := db.Model[Book]().Create(ctx, book); err != nil {
		t.Fatal(err)
	}
	var doomed *Chapter
	for i := range 3 {
		chapter := &Chapter{BookID: book.ID, Title: fmt.Sprintf("Chapter %d", i)}
		if _, err := db.Model[Chapter]().Create(ctx, chapter); err != nil {
			t.Fatal(err)
		}
		doomed = chapter
	}

	if _, err := db.Model[Chapter]().Delete(ctx, doomed); err != nil {
		t.Fatal(err)
	}

	// Direct query hides it.
	direct, err := db.Model[Chapter]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if direct != 2 {
		t.Fatalf("expected 2 visible chapters, got %d", direct)
	}

	// The relation must agree.
	loaded, err := db.Model[Book]().With("Chapters").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 book, got %d", len(loaded))
	}
	if len(loaded[0].Chapters) != 2 {
		t.Fatalf("a soft-deleted chapter must not be eager-loaded, got %d", len(loaded[0].Chapters))
	}
	for _, chapter := range loaded[0].Chapters {
		if chapter.ID == doomed.ID {
			t.Fatal("the deleted chapter came back through the relation")
		}
	}
}
