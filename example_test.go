package orm_test

import (
	"context"
	"database/sql"
	"log"
	"log/slog"
	"time"

	"github.com/lemmego/orm"
)

// The README's examples are written out here so they are compiled on every
// test run. Documentation that does not compile is worse than none: it is
// confidently wrong. These are compile checks, not runnable examples.

type Member struct {
	ID        int    `orm:"primaryKey;autoIncrement"`
	Email     string `orm:"column:email"`
	Name      string `orm:"column:name"`
	Active    bool   `orm:"column:active"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
	Posts     []Article `orm:"hasMany:MemberID"`
	Tags      []Label   `orm:"many2many:member_labels"`
}

type Article struct {
	ID       int `orm:"primaryKey;autoIncrement"`
	MemberID int
	Title    string
}

type Label struct {
	ID   int `orm:"primaryKey;autoIncrement"`
	Name string
}

type MemberEmail struct {
	Email string `orm:"column:email"`
}

var MemberFields = struct {
	ID     orm.Field[Member, int]
	Active orm.Field[Member, bool]
	Email  orm.Field[Member, string]
}{
	ID:     orm.NewField[Member, int]("id"),
	Active: orm.NewField[Member, bool]("active"),
	Email:  orm.NewField[Member, string]("email"),
}

// Setup, as the Quick start section shows it.
func exampleOpen(sqlDB *sql.DB) *orm.DB {
	return orm.Open(sqlDB, orm.Postgres())
}

// Both halves of the predicate API build the same query.
func exampleQueries(ctx context.Context, db *orm.DB) {
	_, _ = db.Model[Member]().
		Where(orm.Eq("active", true)).
		With("Posts").
		OrderBy(orm.Desc("created_at")).
		Limit(20).
		All(ctx)

	_, _ = db.Model[Member]().
		Where(MemberFields.Active.Eq(true)).
		Where(MemberFields.ID.In(1, 2, 3)).
		All(ctx)

	_, _ = db.Model[Member]().Pluck[string](ctx, "email")
	_, _ = db.Model[Member]().Sum[int64](ctx, "id")
	_, _ = db.Model[Member]().As[MemberEmail]().All(ctx)
	_, _ = db.Model[Member]().
		Join[Article](orm.RawPredicate(`"articles"."member_id" = "members"."id"`)).
		As[MemberEmail]().
		All(ctx)
	_, _ = db.Model[Member]().Paginate(ctx, 2, 20)
}

// Soft deletes, upserts and bulk writes.
func exampleWrites(ctx context.Context, db *orm.DB, member *Member) {
	_, _ = db.Model[Member]().Upsert(ctx, member, []string{"email"}, "name", "updated_at")
	_, _ = db.Model[Member]().InsertIgnore(ctx, member, "email")

	_, _ = db.Model[Member]().All(ctx)
	_, _ = db.Model[Member]().WithTrashed().All(ctx)
	_, _ = db.Model[Member]().OnlyTrashed().All(ctx)

	_, _ = db.Model[Member]().Delete(ctx, member)
	_, _ = db.Model[Member]().Restore(ctx, member)
	_, _ = db.Model[Member]().ForceDelete(ctx, member)

	_, _ = db.Model[Member]().Where(orm.Eq("active", false)).UpdateAll(ctx, map[string]any{"active": true})
	_, _ = db.Model[Member]().Where(orm.Eq("active", false)).DeleteAll(ctx)
}

// Associations operate through the database handle, never off the model.
func exampleRelations(ctx context.Context, db *orm.DB, member *Member, post *Article, label *Label) {
	_ = db.Relation[Member, Article](member, "Posts").Add(ctx, post)
	_ = db.Relation[Member, Article](member, "Posts").Load(ctx)
	_, _ = db.Relation[Member, Article](member, "Posts").Count(ctx)

	tags := db.Relation[Member, Label](member, "Tags")
	_ = tags.Attach(ctx, label)
	_ = tags.Sync(ctx, label)
	_ = tags.Toggle(ctx, label)
	_ = tags.Detach(ctx)
	_ = tags.AttachKeys(ctx, 1, 2)
}

// Transactions, savepoints and after-commit callbacks.
func exampleTransaction(ctx context.Context, db *orm.DB, member *Member, post *Article) error {
	return db.Transaction(ctx, func(tx *orm.Tx) error {
		if _, err := tx.Model[Member]().Create(ctx, member); err != nil {
			return err
		}
		if err := tx.Savepoint(ctx, "after_member"); err != nil {
			return err
		}
		if _, err := tx.Model[Article]().Create(ctx, post); err != nil {
			return tx.RollbackTo(ctx, "after_member")
		}
		if err := tx.ReleaseSavepoint(ctx, "after_member"); err != nil {
			return err
		}
		return tx.AfterCommit(func(context.Context) error { return nil })
	})
}

// A transaction the caller finishes itself.
func exampleManualTransaction(ctx context.Context, db *orm.DB) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return tx.Commit()
}

// Both logger forms from the README.
func exampleLogging(sqlDB *sql.DB) {
	_ = orm.Open(sqlDB, orm.Postgres(), orm.WithSlogLogger(slog.Default()))

	_ = orm.Open(sqlDB, orm.Postgres(), orm.WithLogger(
		func(ctx context.Context, e orm.QueryEvent) {
			if e.Duration > 100*time.Millisecond {
				log.Printf("slow: %s", e)
			}
		}))
}

// Scopes compose named filters.
func Active() orm.Scope[Member] {
	return func(q *orm.Query[Member]) *orm.Query[Member] {
		return q.Where(orm.Eq("active", true))
	}
}

func exampleScopes(ctx context.Context, db *orm.DB) {
	_, _ = db.Model[Member]().Scope(Active()).All(ctx)
}

// The constraint helpers documented under errors.
func exampleErrors(err error) bool {
	return orm.IsUniqueViolation(err) || orm.IsForeignKeyViolation(err) ||
		orm.IsNotNullViolation(err) || orm.IsCheckViolation(err)
}

var (
	_ = exampleOpen
	_ = exampleQueries
	_ = exampleWrites
	_ = exampleRelations
	_ = exampleTransaction
	_ = exampleManualTransaction
	_ = exampleLogging
	_ = exampleScopes
	_ = exampleErrors
)
