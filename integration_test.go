package orm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// queryCount records every statement the driver actually runs, which is how
// the eager-loading tests prove they issue one query per relation level
// rather than one per row.
var queryCount atomic.Int64

func init() {
	sql.Register("sqlite3_counting", countingDriver{inner: &sqlite3.SQLiteDriver{}})
}

type countingDriver struct{ inner driver.Driver }

func (d countingDriver) Open(dsn string) (driver.Conn, error) {
	conn, err := d.inner.Open(dsn)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn}, nil
}

type countingConn struct{ driver.Conn }

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if !errors.Is(err, driver.ErrSkip) {
		queryCount.Add(1)
	}
	return rows, err
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

const schema = `
CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email_address TEXT,
    created_at INTEGER,
    legacy_flag TEXT
);
CREATE TABLE posts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER,
    title TEXT
);
CREATE TABLE comments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    post_id INTEGER,
    body TEXT
);
CREATE TABLE hooked_users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT
);
`

// openTestDB returns a fresh in-memory database. The pool is capped at one
// connection because every new connection to ":memory:" would otherwise get
// its own empty database.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite3_counting", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })

	if _, err := sqlDB.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return Open(sqlDB, SQLite())
}

func countQueries(fn func()) int64 {
	queryCount.Store(0)
	fn()
	return queryCount.Load()
}

func TestCreatePopulatesGeneratedKey(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	user := &User{Email: "user@example.com", CreatedAt: 1}
	if _, err := db.Model[User]().Create(ctx, user); err != nil {
		t.Fatal(err)
	}
	if user.ID == 0 {
		t.Fatal("expected the generated primary key to be written back")
	}

	found, err := db.Model[User]().Find(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if found.Email != "user@example.com" {
		t.Fatalf("unexpected round trip: %#v", found)
	}
}

// The table carries a legacy_flag column the struct does not declare. Under
// SELECT * this failed outright; naming the columns makes it a non-event.
func TestUnmappedTableColumnIsIgnored(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO users (email_address, created_at, legacy_flag) VALUES ('a@b.c', 1, 'legacy')`); err != nil {
		t.Fatal(err)
	}
	users, err := db.Model[User]().All(ctx)
	if err != nil {
		t.Fatalf("a column absent from the struct must not break reads: %v", err)
	}
	if len(users) != 1 || users[0].Email != "a@b.c" {
		t.Fatalf("unexpected users: %#v", users)
	}
}

// A NULL column must land in a plain string field as the zero value rather
// than failing the scan.
func TestNullScansIntoNonPointerField(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO users (email_address, created_at) VALUES (NULL, NULL)`); err != nil {
		t.Fatal(err)
	}
	user, err := db.Model[User]().First(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "" || user.CreatedAt != 0 {
		t.Fatalf("expected zero values, got %#v", user)
	}
}

func seedUsersWithPosts(t *testing.T, db *DB, users, postsPer, commentsPer int) {
	t.Helper()
	ctx := context.Background()
	for u := range users {
		user := &User{Email: fmt.Sprintf("user%d@example.com", u), CreatedAt: int64(u)}
		if _, err := db.Model[User]().Create(ctx, user); err != nil {
			t.Fatal(err)
		}
		for p := range postsPer {
			post := &Post{UserID: user.ID, Title: fmt.Sprintf("post %d-%d", u, p)}
			if _, err := db.Model[Post]().Create(ctx, post); err != nil {
				t.Fatal(err)
			}
			for c := range commentsPer {
				comment := &Comment{PostID: post.ID, Body: fmt.Sprintf("comment %d-%d-%d", u, p, c)}
				if _, err := db.Model[Comment]().Create(ctx, comment); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// The acceptance test for milestone 5: eager loading must cost one query per
// relation level, independent of how many rows come back.
func TestEagerLoadingAvoidsNPlusOne(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	seedUsersWithPosts(t, db, 50, 2, 0)

	var users []User
	queries := countQueries(func() {
		var err error
		users, err = db.Model[User]().With("Posts").OrderBy(Asc("id")).All(ctx)
		if err != nil {
			t.Fatal(err)
		}
	})

	if queries != 2 {
		t.Fatalf("expected 2 queries for 50 parents, got %d", queries)
	}
	if len(users) != 50 {
		t.Fatalf("expected 50 users, got %d", len(users))
	}
	for _, user := range users {
		if len(user.Posts) != 2 {
			t.Fatalf("user %d has %d posts, want 2", user.ID, len(user.Posts))
		}
		for _, post := range user.Posts {
			if post.UserID != user.ID {
				t.Fatalf("post %d attached to the wrong user", post.ID)
			}
		}
	}
}

func TestNestedEagerLoading(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	seedUsersWithPosts(t, db, 10, 2, 3)

	var users []User
	queries := countQueries(func() {
		var err error
		users, err = db.Model[User]().With("Posts.Comments").OrderBy(Asc("id")).All(ctx)
		if err != nil {
			t.Fatal(err)
		}
	})

	if queries != 3 {
		t.Fatalf("expected 3 queries for two relation levels, got %d", queries)
	}
	for _, user := range users {
		if len(user.Posts) != 2 {
			t.Fatalf("expected 2 posts, got %d", len(user.Posts))
		}
		for _, post := range user.Posts {
			if len(post.Comments) != 3 {
				t.Fatalf("expected 3 comments on post %d, got %d", post.ID, len(post.Comments))
			}
		}
	}
}

func TestBelongsToEagerLoading(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	seedUsersWithPosts(t, db, 5, 2, 0)

	var posts []Post
	queries := countQueries(func() {
		var err error
		posts, err = db.Model[Post]().With("Author").All(ctx)
		if err != nil {
			t.Fatal(err)
		}
	})
	if queries != 2 {
		t.Fatalf("expected 2 queries, got %d", queries)
	}
	for _, post := range posts {
		if post.Author == nil || post.Author.ID != post.UserID {
			t.Fatalf("post %d has the wrong author: %#v", post.ID, post.Author)
		}
	}
}

func TestEagerLoadingUnknownRelationFails(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	seedUsersWithPosts(t, db, 1, 1, 0)
	if _, err := db.Model[User]().With("Nope").All(ctx); err == nil {
		t.Fatal("expected an error for an undeclared relation")
	}
}

func TestRelationHandleLoadAddRemove(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	user := &User{Email: "owner@example.com"}
	if _, err := db.Model[User]().Create(ctx, user); err != nil {
		t.Fatal(err)
	}

	post := &Post{Title: "attached"}
	if err := db.Relation[User, Post](user, "Posts").Add(ctx, post); err != nil {
		t.Fatal(err)
	}
	if post.ID == 0 || post.UserID != user.ID {
		t.Fatalf("expected the post to be inserted and linked: %#v", post)
	}

	count, err := db.Relation[User, Post](user, "Posts").Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 related post, got %d", count)
	}

	if err := db.Relation[User, Post](user, "Posts").Load(ctx); err != nil {
		t.Fatal(err)
	}
	if len(user.Posts) != 1 || user.Posts[0].Title != "attached" {
		t.Fatalf("expected the relation to be loaded onto the parent: %#v", user.Posts)
	}

	if err := db.Relation[User, Post](user, "Posts").Remove(ctx, post); err != nil {
		t.Fatal(err)
	}
	if post.UserID != 0 {
		t.Fatalf("expected the foreign key to be cleared, got %d", post.UserID)
	}
	// Remove dissociates; it must not delete the row.
	remaining, err := db.Model[Post]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("Remove must not delete rows, found %d", remaining)
	}
}

func TestRelationHandleRejectsWrongChildType(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	user := &User{Email: "a@b.c"}
	if _, err := db.Model[User]().Create(ctx, user); err != nil {
		t.Fatal(err)
	}
	if err := db.Relation[User, Comment](user, "Posts").Add(ctx, &Comment{}); err == nil {
		t.Fatal("expected a type mismatch error")
	}
}

func TestPluckAndAggregates(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	seedUsersWithPosts(t, db, 4, 0, 0)

	emails, err := db.Model[User]().OrderBy(Asc("id")).Pluck[string](ctx, "email_address")
	if err != nil {
		t.Fatal(err)
	}
	if len(emails) != 4 || emails[0] != "user0@example.com" {
		t.Fatalf("unexpected emails: %#v", emails)
	}

	total, err := db.Model[User]().Sum[int64](ctx, "created_at")
	if err != nil {
		t.Fatal(err)
	}
	if total != 0+1+2+3 {
		t.Fatalf("unexpected sum: %d", total)
	}

	maximum, err := db.Model[User]().Max[int64](ctx, "created_at")
	if err != nil {
		t.Fatal(err)
	}
	if maximum != 3 {
		t.Fatalf("unexpected max: %d", maximum)
	}

	// An aggregate over no rows is NULL; it must read back as the zero value.
	empty, err := db.Model[User]().Where(Eq("id", -1)).Sum[int64](ctx, "created_at")
	if err != nil {
		t.Fatal(err)
	}
	if empty != 0 {
		t.Fatalf("expected 0 from an empty aggregate, got %d", empty)
	}
}

// As reprojects the query into a different result type, which a Query[T]
// without generic methods could not express.
func TestAsProjection(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	seedUsersWithPosts(t, db, 3, 0, 0)

	projected, err := db.Model[User]().Where(Gt("created_at", 0)).As[UserEmail]().OrderBy(Asc("id")).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) != 2 || projected[0].Email != "user1@example.com" {
		t.Fatalf("unexpected projection: %#v", projected)
	}

	query, _ := db.Model[User]().As[UserEmail]().SQL()
	if query != `SELECT "email_address" FROM "users"` {
		t.Fatalf("projection must keep the source table: %s", query)
	}
}

// Generic methods must be instantiated before use; a method expression is the
// explicit form of that.
func TestGenericMethodExpression(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	seedUsersWithPosts(t, db, 2, 0, 0)

	pluck := (*Query[User]).Pluck[string]
	emails, err := pluck(db.Model[User]().OrderBy(Asc("id")), ctx, "email_address")
	if err != nil {
		t.Fatal(err)
	}
	if len(emails) != 2 {
		t.Fatalf("unexpected emails: %#v", emails)
	}
}

func TestCreateBatch(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	entities := []*User{
		{Email: "a@example.com", CreatedAt: 1},
		{Email: "b@example.com", CreatedAt: 2},
		{Email: "c@example.com", CreatedAt: 3},
	}
	if _, err := db.Model[User]().CreateBatch(ctx, entities); err != nil {
		t.Fatal(err)
	}
	count, err := db.Model[User]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 rows, got %d", count)
	}
}

func TestBulkUpdateAndDelete(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	seedUsersWithPosts(t, db, 5, 0, 0)

	if _, err := db.Model[User]().Where(Lt("created_at", 2)).UpdateAll(ctx, map[string]any{"email_address": "bulk@example.com"}); err != nil {
		t.Fatal(err)
	}
	updated, err := db.Model[User]().Where(Eq("email_address", "bulk@example.com")).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if updated != 2 {
		t.Fatalf("expected 2 updated rows, got %d", updated)
	}

	if _, err := db.Model[User]().Where(Gte("created_at", 3)).DeleteAll(ctx); err != nil {
		t.Fatal(err)
	}
	remaining, err := db.Model[User]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 3 {
		t.Fatalf("expected 3 remaining rows, got %d", remaining)
	}
}

func TestExistsAndFirstNotFound(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()

	exists, err := db.Model[User]().Where(Eq("id", 1)).Exists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expected no rows")
	}
	if _, err := db.Model[User]().First(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUniqueConstraintIsClassified(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	if _, err := db.Exec(ctx, `CREATE UNIQUE INDEX users_email ON users (email_address)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[User]().Create(ctx, &User{Email: "dup@example.com"}); err != nil {
		t.Fatal(err)
	}

	_, err := db.Model[User]().Create(ctx, &User{Email: "dup@example.com"})
	var constraint *ConstraintError
	if !errors.As(err, &constraint) {
		t.Fatalf("expected a ConstraintError, got %v", err)
	}
	if constraint.Kind != ConstraintUnique {
		t.Fatalf("expected a unique violation, got %s", constraint.Kind)
	}
	if constraint.Table != "users" || len(constraint.Columns) != 1 || constraint.Columns[0] != "email_address" {
		t.Fatalf("expected the offending column to be reported: %#v", constraint)
	}
	if !IsUniqueViolation(err) {
		t.Fatal("expected IsUniqueViolation to report true")
	}
}

func TestRawQueryToleratesExtraColumns(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	seedUsersWithPosts(t, db, 1, 0, 0)

	users, err := db.Raw[User](`SELECT id, email_address, created_at, legacy_flag FROM users`).All(ctx)
	if err != nil {
		t.Fatalf("raw queries must tolerate unmapped columns: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("unexpected users: %#v", users)
	}
}

type HookedUser struct {
	ID     int `orm:"primaryKey;autoIncrement"`
	Name   string
	Events *[]string `orm:"-"`
}

func (u *HookedUser) BeforeCreate(context.Context) error {
	*u.Events = append(*u.Events, "before_create")
	return nil
}

func (u *HookedUser) AfterCreate(context.Context) error {
	*u.Events = append(*u.Events, "after_create")
	return nil
}

func TestHookOrderAndAfterCommit(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()

	events := []string{}
	if _, err := db.Model[HookedUser]().Create(ctx, &HookedUser{Name: "first", Events: &events}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0] != "before_create" || events[1] != "after_create" {
		t.Fatalf("unexpected hook order: %#v", events)
	}

	committed := false
	if err := db.Transaction(ctx, func(tx *Tx) error {
		return tx.AfterCommit(func(context.Context) error { committed = true; return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("expected the after-commit callback to run")
	}
}

func TestTransactionRollbackAndPanic(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()

	called := false
	err := db.Transaction(ctx, func(tx *Tx) error {
		if err := tx.AfterCommit(func(context.Context) error { called = true; return nil }); err != nil {
			return err
		}
		if _, err := tx.Model[User]().Create(ctx, &User{Email: "rolled@back"}); err != nil {
			return err
		}
		return context.Canceled
	})
	if err == nil || called {
		t.Fatalf("expected rollback without callbacks: %v %v", err, called)
	}

	count, err := db.Model[User]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected the insert to be rolled back, found %d rows", count)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the panic to propagate")
			}
		}()
		_ = db.Transaction(ctx, func(*Tx) error { panic("boom") })
	}()
}

func TestTxModelSharesTheTransaction(t *testing.T) {
	db, ctx := openTestDB(t), context.Background()
	if err := db.Transaction(ctx, func(tx *Tx) error {
		if _, err := tx.Model[User]().Create(ctx, &User{Email: "in@tx"}); err != nil {
			return err
		}
		count, err := tx.Model[User]().Count(ctx)
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("expected the row to be visible inside the transaction, got %d", count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	count, err := db.Model[User]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected the commit to persist, got %d", count)
	}
}
