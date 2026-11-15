package orm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// collector records what the observers saw.
type collector struct {
	mu      sync.Mutex
	changes []Change
}

func (c *collector) observe(_ context.Context, change Change) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.changes = append(c.changes, change)
}

func (c *collector) all() []Change {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Change(nil), c.changes...)
}

func (c *collector) ops() string {
	parts := make([]string, 0, len(c.all()))
	for _, change := range c.all() {
		parts = append(parts, change.Op.String()+":"+change.Table)
	}
	return strings.Join(parts, " ")
}

func observed(t *testing.T) (*DB, *collector) {
	t.Helper()
	db := openMilestone6DB(t)
	seen := &collector{}
	db.Observe(seen.observe)
	return db, seen
}

// TestAnObserverSeesEveryWrite, with the operation and the table it landed in.
func TestAnObserverSeesEveryWrite(t *testing.T) {
	db, seen := observed(t)
	ctx := context.Background()

	article := &Article{Title: "observed"}
	if _, err := db.Model[Article]().Create(ctx, article); err != nil {
		t.Fatal(err)
	}
	article.Title = "renamed"
	if _, err := db.Model[Article]().Update(ctx, article); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[Article]().ForceDelete(ctx, article); err != nil {
		t.Fatal(err)
	}

	if got := seen.ops(); got != "create:articles update:articles delete:articles" {
		t.Fatalf("the observer saw %q", got)
	}
	// The entity is the pointer the caller passed, so the generated key is
	// already on it — which is what makes an observer able to key a document
	// by primary key at all.
	for _, change := range seen.all() {
		article, ok := change.Entity.(*Article)
		if !ok {
			t.Fatalf("the entity arrived as %T", change.Entity)
		}
		if article.ID == 0 {
			t.Error("the entity reached the observer without its generated key")
		}
	}
}

// TestAnObserverSeesOnlyCommittedChanges, in both directions.
//
// The rollback half is the one that matters: a change announced inside the
// transaction tells an observer about a row that then ceases to exist, and
// nothing afterwards can take it back. The commit half is what stops the
// rollback assertion passing for an implementation that simply never emits.
func TestAnObserverSeesOnlyCommittedChanges(t *testing.T) {
	db, seen := observed(t)
	ctx := context.Background()

	rolled := errors.New("no")
	err := db.Transaction(ctx, func(tx *Tx) error {
		if _, err := tx.Model[Article]().Create(ctx, &Article{Title: "doomed"}); err != nil {
			return err
		}
		if len(seen.all()) != 0 {
			t.Error("the observer was told inside the transaction, before the " +
				"row was visible to anything and before it was certain to exist")
		}
		return rolled
	})
	if !errors.Is(err, rolled) {
		t.Fatalf("got %v", err)
	}
	if got := seen.all(); len(got) != 0 {
		t.Fatalf("a rolled-back write was announced: %v", got)
	}

	if err := db.Transaction(ctx, func(tx *Tx) error {
		_, err := tx.Model[Article]().Create(ctx, &Article{Title: "kept"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := seen.ops(); got != "create:articles" {
		t.Fatalf("a committed write was announced as %q", got)
	}
}

// TestAnObserverRunsAfterTheRowIsVisibleToAnotherConnection.
//
// After-commit is only worth having if it means this: the observer can go and
// read what it was told about. So the read goes through a second, independent
// pool — not the handle that wrote it, which would see its own uncommitted
// work and prove nothing.
//
// It needs a file-backed database, because every connection to ":memory:"
// gets its own empty one, which is why the rest of this suite caps its pool
// at a single connection.
func TestAnObserverRunsAfterTheRowIsVisibleToAnotherConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observe.db")

	writer, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	if _, err := writer.Exec(schema6); err != nil {
		t.Fatal(err)
	}

	reader, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })

	db := Open(writer, SQLite())

	var (
		found int
		asked error
	)
	db.Observe(func(ctx context.Context, change Change) {
		article := change.Entity.(*Article)
		asked = reader.QueryRowContext(ctx,
			`SELECT count(*) FROM articles WHERE id = ?`, article.ID).Scan(&found)
	})

	ctx := context.Background()
	if err := db.Transaction(ctx, func(tx *Tx) error {
		_, err := tx.Model[Article]().Create(ctx, &Article{Title: "visible"})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if asked != nil {
		t.Fatalf("the observer could not read the row: %v", asked)
	}
	if found != 1 {
		t.Fatal("the observer ran before the row was visible to another " +
			"connection, which is the whole guarantee: a subscriber that " +
			"refetches on being told would find nothing")
	}
}

// TestASoftDeleteIsObservedAsADelete, although the statement is an UPDATE.
//
// What a soft delete means downstream is that the row is gone. An observer
// told "updated" would reindex a row it should have removed, which is a
// disclosure rather than a stale cache.
func TestASoftDeleteIsObservedAsADelete(t *testing.T) {
	db, seen := observed(t)
	ctx := context.Background()

	article := &Article{Title: "soft"}
	if _, err := db.Model[Article]().Create(ctx, article); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[Article]().Delete(ctx, article); err != nil {
		t.Fatal(err)
	}

	if got := seen.ops(); got != "create:articles delete:articles" {
		t.Fatalf("the observer saw %q, want the soft delete reported as a delete", got)
	}
}

// TestARestoreIsObserved, because a restored row that nothing announces stays
// missing from every index that removed it on the delete, and the only way
// back is a reimport.
//
// Restore runs no AfterUpdate entity hook, which is a pre-existing asymmetry;
// this asserts the observer does not inherit it.
func TestARestoreIsObserved(t *testing.T) {
	db, seen := observed(t)
	ctx := context.Background()

	article := &Article{Title: "back"}
	if _, err := db.Model[Article]().Create(ctx, article); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[Article]().Delete(ctx, article); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[Article]().Restore(ctx, article); err != nil {
		t.Fatal(err)
	}

	if got := seen.ops(); got != "create:articles delete:articles update:articles" {
		t.Fatalf("the observer saw %q, want the restore among them", got)
	}
}

// TestUpdateAllAndDeleteAllObserveNothing, which is a hole asserted rather
// than a bug fixed.
//
// There is no entity to announce, and the matched set is unknown without a
// RETURNING or a prior SELECT that changes the statement's cost and its lock
// footprint. A loud nothing beats a surprise query, so the limitation is
// pinned here: a bulk mutation is reconciled by a reimport, not by this.
func TestUpdateAllAndDeleteAllObserveNothing(t *testing.T) {
	db, seen := observed(t)
	ctx := context.Background()

	for i := range 3 {
		if _, err := db.Model[Article]().Create(ctx,
			&Article{Title: fmt.Sprintf("bulk %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	before := len(seen.all())

	if _, err := db.Model[Article]().
		Where(RawPredicate(`"views" = ?`, 0)).
		UpdateAll(ctx, map[string]any{"views": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[Article]().
		Where(RawPredicate(`"views" = ?`, 1)).
		DeleteAll(ctx); err != nil {
		t.Fatal(err)
	}

	if got := len(seen.all()); got != before {
		t.Fatalf("a bulk mutation announced %d changes; UpdateAll and DeleteAll "+
			"observe nothing, and a test that let that change silently would "+
			"turn a documented limitation into a surprise", got-before)
	}
}

// TestAPanickingObserverDoesNotTakeTheWriteDown, and does not stop the ones
// after it.
func TestAPanickingObserverDoesNotTakeTheWriteDown(t *testing.T) {
	db := openMilestone6DB(t)
	seen := &collector{}
	db.Observe(func(context.Context, Change) { panic("observer is broken") })
	db.Observe(seen.observe)

	ctx := context.Background()
	if _, err := db.Model[Article]().Create(ctx, &Article{Title: "survives"}); err != nil {
		t.Fatalf("a panicking observer failed the write: %v", err)
	}
	if len(seen.all()) != 1 {
		t.Error("a panicking observer stopped the ones registered after it")
	}
}

// TestACommitWhoseCallbackFailsIsDistinguishableFromAFailedCommit.
//
// Both used to return a bare error, and the natural response to a commit
// error is to retry the unit of work — which for a transaction that actually
// committed applies it twice.
func TestACommitWhoseCallbackFailsIsDistinguishableFromAFailedCommit(t *testing.T) {
	db := openMilestone6DB(t)
	ctx := context.Background()

	var ran int
	first, second := errors.New("first"), errors.New("second")
	err := db.Transaction(ctx, func(tx *Tx) error {
		if _, err := tx.Model[Article]().Create(ctx, &Article{Title: "committed"}); err != nil {
			return err
		}
		if err := tx.AfterCommit(func(context.Context) error { ran++; return first }); err != nil {
			return err
		}
		return tx.AfterCommit(func(context.Context) error { ran++; return second })
	})

	if !errors.Is(err, ErrAfterCommit) {
		t.Fatalf("got %v, want it to wrap ErrAfterCommit", err)
	}
	// Both callbacks' errors, not just the first — and both callbacks ran.
	// Returning on the first would silently skip work that was registered
	// precisely because it has to happen after the commit.
	if !errors.Is(err, first) || !errors.Is(err, second) {
		t.Errorf("got %v, want both callbacks' errors joined", err)
	}
	if ran != 2 {
		t.Errorf("%d of 2 callbacks ran", ran)
	}

	// And the row is there, which is what makes the distinction worth having.
	count, err := db.Model[Article]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("the transaction reported an error and wrote %d rows", count)
	}
}

// TestAnObserverIsPerHandleAndNotPerProcess.
//
// This is why Observe is a method and not a package-level registry. A global
// one cannot be cleaned up between test cases, so one case's observer
// satisfies the next case's assertion — a registry that manufactures a
// vacuous test.
func TestAnObserverIsPerHandleAndNotPerProcess(t *testing.T) {
	first := openMilestone6DB(t)
	second := Open(first.SQLDB(), SQLite())

	watched := &collector{}
	first.Observe(watched.observe)

	ctx := context.Background()
	if _, err := second.Model[Article]().Create(ctx, &Article{Title: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if got := watched.all(); len(got) != 0 {
		t.Fatalf("an observer on one handle saw another handle's write: %v", got)
	}
	if first.Observers() != 1 || second.Observers() != 0 {
		t.Errorf("observers: first %d, second %d", first.Observers(), second.Observers())
	}
}

// TestARelationWriteIsObserved, so attaching a child is not a silent write.
func TestARelationWriteIsObserved(t *testing.T) {
	db := openTestDB(t)
	seen := &collector{}
	db.Observe(seen.observe)

	ctx := context.Background()
	user := &User{Email: "relation@example.com"}
	if _, err := db.Model[User]().Create(ctx, user); err != nil {
		t.Fatal(err)
	}
	if err := db.Relation[User, Post](user, "Posts").
		Add(ctx, &Post{Title: "attached"}); err != nil {
		t.Fatal(err)
	}

	if got := seen.ops(); got != "create:users create:posts" {
		t.Fatalf("the observer saw %q, want the relation write among them", got)
	}
}

// TestNoObserverIsFree, so the field costs nothing to an application that
// never registers one.
func TestNoObserverIsFree(t *testing.T) {
	db := openMilestone6DB(t)
	if db.Observers() != 0 {
		t.Fatal("a fresh handle has observers")
	}
	db.Observe(nil)
	if db.Observers() != 0 {
		t.Error("a nil observer was registered, so every write would call it")
	}
	if _, err := db.Model[Article]().Create(context.Background(),
		&Article{Title: "unobserved"}); err != nil {
		t.Fatal(err)
	}
}
