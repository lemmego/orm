package orm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

type Article struct {
	ID        int `orm:"primaryKey;autoIncrement"`
	Title     string
	Views     int
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
	Tags      []Tag `orm:"many2many:article_tags"`
}

type Tag struct {
	ID       int `orm:"primaryKey;autoIncrement"`
	Name     string
	Articles []Article `orm:"many2many:article_tags"`
}

// Ticket uses sql.NullTime rather than a pointer for its soft-delete column.
type Ticket struct {
	ID        int `orm:"primaryKey;autoIncrement"`
	Subject   string
	DeletedAt sql.NullTime
}

// BadSoftDelete asks for soft deletes on a column that can never be NULL.
type BadSoftDelete struct {
	ID      int       `orm:"primaryKey;autoIncrement"`
	Removed time.Time `orm:"softDelete"`
}

// ManualStamp keeps an int64 CreatedAt, which must stay under manual control.
type ManualStamp struct {
	ID        int `orm:"primaryKey;autoIncrement"`
	CreatedAt int64
}

const schema6 = `
CREATE TABLE articles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT,
    views INTEGER,
    created_at DATETIME,
    updated_at DATETIME,
    deleted_at DATETIME
);
CREATE TABLE tags (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT
);
CREATE TABLE article_tags (
    article_id INTEGER,
    tag_id INTEGER
);
CREATE TABLE tickets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    subject TEXT,
    deleted_at DATETIME
);
`

func openMilestone6DB(t *testing.T) *DB {
	t.Helper()
	db := openTestDB(t)
	if _, err := db.SQLDB().Exec(schema6); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedArticles(t *testing.T, db *DB, count int) []*Article {
	t.Helper()
	ctx := context.Background()
	articles := make([]*Article, 0, count)
	for i := range count {
		article := &Article{Title: fmt.Sprintf("article %d", i), Views: i * 10}
		if _, err := db.Model[Article]().Create(ctx, article); err != nil {
			t.Fatal(err)
		}
		articles = append(articles, article)
	}
	return articles
}

// --- soft deletes ---------------------------------------------------------

func TestSoftDeleteHidesRowsFromReads(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	articles := seedArticles(t, db, 3)

	if _, err := db.Model[Article]().Delete(ctx, articles[0]); err != nil {
		t.Fatal(err)
	}
	if articles[0].DeletedAt == nil {
		t.Fatal("expected the soft-delete marker to be set on the entity")
	}

	// The row must still be there, just hidden.
	var raw int
	if err := db.SQLDB().QueryRow(`SELECT COUNT(*) FROM articles`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != 3 {
		t.Fatalf("soft delete must not remove the row, found %d", raw)
	}

	visible, err := db.Model[Article]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if visible != 2 {
		t.Fatalf("expected 2 visible rows, got %d", visible)
	}

	all, err := db.Model[Article]().WithTrashed().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if all != 3 {
		t.Fatalf("WithTrashed should see 3 rows, got %d", all)
	}

	trashed, err := db.Model[Article]().OnlyTrashed().All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(trashed) != 1 || trashed[0].ID != articles[0].ID {
		t.Fatalf("OnlyTrashed returned %#v", trashed)
	}

	if _, err := db.Model[Article]().Find(ctx, articles[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a soft-deleted row must not be findable, got %v", err)
	}
}

func TestRestoreAndForceDelete(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	articles := seedArticles(t, db, 2)

	if _, err := db.Model[Article]().Delete(ctx, articles[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[Article]().Restore(ctx, articles[0]); err != nil {
		t.Fatal(err)
	}
	if articles[0].DeletedAt != nil {
		t.Fatal("expected the marker to be cleared on the entity")
	}
	count, err := db.Model[Article]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected the row to be restored, got %d visible", count)
	}

	if _, err := db.Model[Article]().ForceDelete(ctx, articles[1]); err != nil {
		t.Fatal(err)
	}
	var raw int
	if err := db.SQLDB().QueryRow(`SELECT COUNT(*) FROM articles`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != 1 {
		t.Fatalf("ForceDelete must remove the row, found %d", raw)
	}
}

func TestBulkSoftDeleteAndForceDeleteAll(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	seedArticles(t, db, 5)

	if _, err := db.Model[Article]().Where(Lt("views", 30)).DeleteAll(ctx); err != nil {
		t.Fatal(err)
	}
	visible, err := db.Model[Article]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if visible != 2 {
		t.Fatalf("expected 2 visible rows, got %d", visible)
	}
	var raw int
	if err := db.SQLDB().QueryRow(`SELECT COUNT(*) FROM articles`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != 5 {
		t.Fatalf("DeleteAll must soft delete, found %d rows", raw)
	}

	// ForceDeleteAll still honours the default scope, so it purges only the
	// rows that were visible and leaves the trashed ones behind.
	if _, err := db.Model[Article]().Where(Gte("views", 0)).ForceDeleteAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.SQLDB().QueryRow(`SELECT COUNT(*) FROM articles`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != 3 {
		t.Fatalf("expected the 3 trashed rows to survive, found %d", raw)
	}

	// WithTrashed is how you purge everything.
	if _, err := db.Model[Article]().WithTrashed().Where(Gte("views", 0)).ForceDeleteAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.SQLDB().QueryRow(`SELECT COUNT(*) FROM articles`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != 0 {
		t.Fatalf("expected WithTrashed().ForceDeleteAll() to purge everything, found %d", raw)
	}
}

func TestSoftDeleteWithNullTime(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	ticket := &Ticket{Subject: "printer on fire"}
	if _, err := db.Model[Ticket]().Create(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Model[Ticket]().Delete(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	if !ticket.DeletedAt.Valid {
		t.Fatal("expected the NullTime marker to be valid")
	}
	count, err := db.Model[Ticket]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected the ticket to be hidden, got %d", count)
	}
}

// A column that cannot hold NULL can never satisfy "deleted_at IS NULL", so
// asking for soft deletes on one is refused rather than silently hiding every
// row in the table.
func TestSoftDeleteRequiresANullableColumn(t *testing.T) {
	_, err := ModelInfoFor[BadSoftDelete]()
	if err == nil {
		t.Fatal("expected a non-nullable soft-delete column to be rejected")
	}
}

// --- automatic timestamps -------------------------------------------------

func TestAutomaticTimestamps(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()

	article := &Article{Title: "stamped"}
	if _, err := db.Model[Article]().Create(ctx, article); err != nil {
		t.Fatal(err)
	}
	if article.CreatedAt.IsZero() || article.UpdatedAt.IsZero() {
		t.Fatalf("expected both timestamps to be stamped: %#v", article)
	}
	created := article.CreatedAt

	time.Sleep(2 * time.Millisecond)
	article.Title = "restamped"
	if _, err := db.Model[Article]().Update(ctx, article); err != nil {
		t.Fatal(err)
	}
	if !article.UpdatedAt.After(created) {
		t.Fatal("expected UpdatedAt to move forward on update")
	}
	if !article.CreatedAt.Equal(created) {
		t.Fatal("expected CreatedAt to stay put")
	}

	stored, err := db.Model[Article]().Find(ctx, article.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CreatedAt.IsZero() {
		t.Fatal("expected the timestamp to round-trip through the database")
	}
}

func TestExplicitTimestampIsPreserved(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	chosen := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

	article := &Article{Title: "backdated", CreatedAt: chosen}
	if _, err := db.Model[Article]().Create(ctx, article); err != nil {
		t.Fatal(err)
	}
	if !article.CreatedAt.Equal(chosen) {
		t.Fatalf("an explicitly set CreatedAt must not be overwritten, got %s", article.CreatedAt)
	}
}

// A non-time CreatedAt stays entirely under the caller's control.
func TestNonTimeTimestampFieldIsLeftAlone(t *testing.T) {
	info := mustInfo[ManualStamp](t)
	if info.CreatedAt != nil {
		t.Fatal("an int64 CreatedAt must not be treated as an automatic timestamp")
	}
	if userInfo := mustInfo[User](t); userInfo.CreatedAt != nil {
		t.Fatal("User.CreatedAt is an int64 and must stay manual")
	}
}

// --- pagination -----------------------------------------------------------

func TestPagination(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	seedArticles(t, db, 7)

	page, err := db.Model[Article]().OrderBy(Asc("id")).Paginate(ctx, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 7 || page.TotalPages != 3 || page.Page != 2 || page.PerPage != 3 {
		t.Fatalf("unexpected page metadata: %#v", page)
	}
	if len(page.Items) != 3 || page.Items[0].Title != "article 3" {
		t.Fatalf("unexpected page contents: %#v", page.Items)
	}
	if !page.HasNext() || !page.HasPrev() {
		t.Fatal("page 2 of 3 has both neighbours")
	}

	last, err := db.Model[Article]().OrderBy(Asc("id")).Paginate(ctx, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Items) != 1 || last.HasNext() {
		t.Fatalf("unexpected final page: %#v", last)
	}

	// Total counts the whole result set, not the page.
	if last.Total != 7 {
		t.Fatalf("expected total 7, got %d", last.Total)
	}
}

func TestPaginationExcludesSoftDeleted(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	articles := seedArticles(t, db, 4)
	if _, err := db.Model[Article]().Delete(ctx, articles[0]); err != nil {
		t.Fatal(err)
	}
	page, err := db.Model[Article]().Paginate(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || len(page.Items) != 3 {
		t.Fatalf("soft-deleted rows must not be counted: %#v", page)
	}
}

// --- scopes ---------------------------------------------------------------

func popular() Scope[Article] {
	return func(q *Query[Article]) *Query[Article] { return q.Where(Gte("views", 30)) }
}

func titled(title string) Scope[Article] {
	return func(q *Query[Article]) *Query[Article] { return q.Where(Eq("title", title)) }
}

func TestScopesCompose(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	seedArticles(t, db, 5)

	count, err := db.Model[Article]().Scope(popular()).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 popular articles, got %d", count)
	}

	articles, err := db.Model[Article]().Scope(popular(), titled("article 4")).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(articles) != 1 || articles[0].Title != "article 4" {
		t.Fatalf("unexpected scoped result: %#v", articles)
	}
}

// --- many to many ---------------------------------------------------------

func seedTags(t *testing.T, db *DB, names ...string) []*Tag {
	t.Helper()
	ctx := context.Background()
	tags := make([]*Tag, 0, len(names))
	for _, name := range names {
		tag := &Tag{Name: name}
		if _, err := db.Model[Tag]().Create(ctx, tag); err != nil {
			t.Fatal(err)
		}
		tags = append(tags, tag)
	}
	return tags
}

func TestManyToManyAttachDetachSyncToggle(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	article := seedArticles(t, db, 1)[0]
	tags := seedTags(t, db, "go", "sql", "orm")

	handle := func() *RelationHandle[Article, Tag] {
		return db.Relation[Article, Tag](article, "Tags")
	}

	if err := handle().Attach(ctx, tags[0], tags[1]); err != nil {
		t.Fatal(err)
	}
	count, err := handle().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 tags attached, got %d", count)
	}

	// Attach is idempotent: re-attaching must not duplicate pivot rows.
	if err := handle().Attach(ctx, tags[0]); err != nil {
		t.Fatal(err)
	}
	var pivotRows int
	if err := db.SQLDB().QueryRow(`SELECT COUNT(*) FROM article_tags`).Scan(&pivotRows); err != nil {
		t.Fatal(err)
	}
	if pivotRows != 2 {
		t.Fatalf("re-attaching duplicated pivot rows: %d", pivotRows)
	}

	if err := handle().Detach(ctx, tags[0]); err != nil {
		t.Fatal(err)
	}
	if count, _ = handle().Count(ctx); count != 1 {
		t.Fatalf("expected 1 tag after detach, got %d", count)
	}

	// Sync makes the set exactly what was asked for.
	if err := handle().Sync(ctx, tags[1], tags[2]); err != nil {
		t.Fatal(err)
	}
	names, err := handle().Query().OrderBy(Asc("name")).Pluck[string](ctx, "name")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "orm" || names[1] != "sql" {
		t.Fatalf("unexpected synced tags: %#v", names)
	}

	// Toggle flips membership: sql is present so it goes, go is absent so it comes.
	if err := handle().Toggle(ctx, tags[1], tags[0]); err != nil {
		t.Fatal(err)
	}
	names, err = handle().Query().OrderBy(Asc("name")).Pluck[string](ctx, "name")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "go" || names[1] != "orm" {
		t.Fatalf("unexpected toggled tags: %#v", names)
	}

	// Detaching with no arguments clears the association.
	if err := handle().Detach(ctx); err != nil {
		t.Fatal(err)
	}
	if count, _ = handle().Count(ctx); count != 0 {
		t.Fatalf("expected the association to be cleared, got %d", count)
	}
}

// Eager loading a many-to-many costs one pivot query plus one target query,
// on top of the root query, no matter how many parents there are.
func TestManyToManyEagerLoadingQueryCount(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	articles := seedArticles(t, db, 20)
	tags := seedTags(t, db, "go", "sql")
	for _, article := range articles {
		if err := db.Relation[Article, Tag](article, "Tags").Attach(ctx, tags[0], tags[1]); err != nil {
			t.Fatal(err)
		}
	}

	var loaded []Article
	queries := countQueries(func() {
		var err error
		loaded, err = db.Model[Article]().With("Tags").OrderBy(Asc("id")).All(ctx)
		if err != nil {
			t.Fatal(err)
		}
	})
	if queries != 3 {
		t.Fatalf("expected 3 queries for 20 parents, got %d", queries)
	}
	if len(loaded) != 20 {
		t.Fatalf("expected 20 articles, got %d", len(loaded))
	}
	for _, article := range loaded {
		if len(article.Tags) != 2 {
			t.Fatalf("article %d has %d tags, want 2", article.ID, len(article.Tags))
		}
	}
}

func TestManyToManyLoadsFromBothSides(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	articles := seedArticles(t, db, 2)
	tags := seedTags(t, db, "shared")
	for _, article := range articles {
		if err := db.Relation[Article, Tag](article, "Tags").Attach(ctx, tags[0]); err != nil {
			t.Fatal(err)
		}
	}

	loaded, err := db.Model[Tag]().With("Articles").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || len(loaded[0].Articles) != 2 {
		t.Fatalf("expected the inverse side to load 2 articles: %#v", loaded)
	}
}

func TestManyToManyRelationLoadOntoParent(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	article := seedArticles(t, db, 1)[0]
	tags := seedTags(t, db, "go")
	if err := db.Relation[Article, Tag](article, "Tags").Attach(ctx, tags[0]); err != nil {
		t.Fatal(err)
	}
	if err := db.Relation[Article, Tag](article, "Tags").Load(ctx); err != nil {
		t.Fatal(err)
	}
	if len(article.Tags) != 1 || article.Tags[0].Name != "go" {
		t.Fatalf("expected the relation to load onto the parent: %#v", article.Tags)
	}
}

func TestManyToManyWorksInsideTransaction(t *testing.T) {
	db, ctx := openMilestone6DB(t), context.Background()
	article := seedArticles(t, db, 1)[0]
	tags := seedTags(t, db, "go")

	err := db.Transaction(ctx, func(tx *Tx) error {
		if err := tx.Relation[Article, Tag](article, "Tags").Attach(ctx, tags[0]); err != nil {
			return err
		}
		return errors.New("roll back")
	})
	if err == nil {
		t.Fatal("expected the transaction to fail")
	}
	count, err := db.Relation[Article, Tag](article, "Tags").Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected the attach to roll back, got %d", count)
	}
}
