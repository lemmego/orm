package orm_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lemmego/orm"
)

// The ORM section of the docs site, compiled.
//
// Every predicate, query chain, write, relation and pagination call the
// pages at /docs/orm make, with the arity they are documented with. The
// migration docs shipped four years of calls that did not exist because
// nothing ever compiled them; this is the cheap half of not repeating that.
//
// Nothing here opens a database. What is being checked is the signature,
// which the compiler settles without one.

type docUser struct {
	ID        int        `orm:"primaryKey;autoIncrement"`
	Name      string     `orm:"column:name"`
	Email     string     `orm:"column:email"`
	Active    bool       `orm:"column:active"`
	Age       int        `orm:"column:age"`
	CreatedAt time.Time  `orm:"column:created_at"`
	DeletedAt *time.Time `orm:"column:deleted_at"`
	Posts     []docPost  `orm:"hasMany:UserID"`
}

type docPost struct {
	ID        int    `orm:"primaryKey;autoIncrement"`
	UserID    int    `orm:"column:user_id"`
	Title     string `orm:"column:title"`
	Published bool   `orm:"column:published"`
}

// queries.md and _index.md: the predicate vocabulary.
func TestDocsPredicates(t *testing.T) {
	for _, p := range []orm.Predicate{
		orm.Eq("active", true),
		orm.Ne("role", "admin"),
		orm.Gt("age", 18),
		orm.Gte("age", 18),
		orm.Lt("age", 65),
		orm.Lte("age", 65),
		orm.IsNull("deleted_at"),
		orm.IsNotNull("verified_at"),
		orm.Between("age", 18, 65),
		orm.Like("name", "%ada%"),
		orm.In("id", 1, 2, 3),
		orm.And(orm.Eq("active", true), orm.Gt("age", 18)),
		orm.Or(orm.Eq("role", "admin"), orm.Eq("role", "owner")),
		orm.Not(orm.Eq("banned", true)),
		orm.RawPredicate(`(published_at, id) < (?, ?)`, time.Now(), 1),
	} {
		// A zero Predicate compiles to a WHERE that matches nothing, which
		// is the failure worth catching: a helper that quietly built one
		// would turn a filtered query into an empty result.
		if p.IsZero() {
			t.Error("a documented predicate built the zero value, which matches nothing")
		}
	}

	// Ordering helpers.
	_ = orm.Desc("created_at")
	_ = orm.Asc("name")
}

// The query chain the pages build, compiled against the real builder.
func documentedQueryChain(db *orm.DB, ctx context.Context) {
	_, _ = db.Model[docUser]().
		Where(orm.Eq("active", true)).
		With("Posts").
		OrderBy(orm.Desc("created_at")).
		Limit(20).
		Offset(0).
		All(ctx)

	_, _ = db.Model[docUser]().Where(orm.Eq("id", 1)).First(ctx)
	_, _ = db.Model[docUser]().Count(ctx)
	_, _ = db.Model[docUser]().Exists(ctx)

	// Generic methods on a concrete type, which is what lets the result type
	// change — and the reason Query is concrete rather than an interface.
	_, _ = db.Model[docUser]().Pluck[string](ctx, "email")
	_, _ = db.Model[docUser]().Sum[int64](ctx, "age")

	// Soft deletes.
	_, _ = db.Model[docUser]().WithTrashed().All(ctx)
	_, _ = db.Model[docUser]().OnlyTrashed().All(ctx)

	// Pagination.
	page, _ := db.Model[docUser]().Paginate(ctx, 1, 20)
	if page != nil {
		_ = page.HasNext()
		_ = page.HasPrev()
	}
}

// writes.md
func documentedWrites(db *orm.DB, ctx context.Context) {
	user := &docUser{Name: "Ada"}

	_, _ = db.Model[docUser]().Create(ctx, user)
	_, _ = db.Model[docUser]().CreateBatch(ctx, []*docUser{user})
	_, _ = db.Model[docUser]().Update(ctx, user)
	_, _ = db.Model[docUser]().Delete(ctx, user)

	// Bulk mutations refuse to run without a predicate.
	_, _ = db.Model[docUser]().
		Where(orm.Lt("last_seen", time.Now())).
		UpdateAll(ctx, map[string]any{"active": false})
	_, _ = db.Model[docUser]().
		Where(orm.Eq("active", false)).
		DeleteAll(ctx)

	// The sentinels the page names.
	_ = errors.Is(nil, orm.ErrNotFound)
	_ = errors.Is(nil, orm.ErrUnsafeMutation)

	var constraint *orm.ConstraintError
	_ = errors.As(errors.New("x"), &constraint)
	_ = orm.IsUniqueViolation(errors.New("x"))
}

// relationships.md
func documentedRelations(db *orm.DB, ctx context.Context) {
	user := &docUser{ID: 1}
	post := &docPost{ID: 1}

	// A generic method on *DB, so the two type parameters are explicit and
	// the handle is typed on both sides.
	rel := db.Relation[docUser, docPost](user, "Posts")
	_ = rel.Add(ctx, post)
	_ = rel.Remove(ctx, post)
	_ = rel.Load(ctx)
}

func TestDocsOrmSurfaceCompiles(t *testing.T) {
	// Referenced rather than called: each needs a live connection, and the
	// signature is what is being checked.
	_ = documentedQueryChain
	_ = documentedWrites
	_ = documentedRelations
}
