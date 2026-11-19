# Lemmego ORM

An independent SQL persistence module built directly on `database/sql`.

**It stands alone.** Its only dependencies are `database/sql` and two small
naming libraries (`go-pluralize`, `strcase`) — shared with the CLI and
migration modules so that table names agree. It imports no other Lemmego
package, and nothing here needs GPA, a connector, or the framework:

```go
db := orm.Open(sqlDB, orm.Postgres())
```

is the whole setup. Two optional modules build on it, and neither is required:
[`gpaorm`](https://github.com/lemmego/gpaorm) exposes it through the GPA contracts, and
[`ormconnector`](https://github.com/lemmego/ormconnector) wires it into the application lifecycle. The
dependency direction is one-way — they import the ORM, never the reverse.

## Quick start

```go
type User struct {
    ID        int       `orm:"primaryKey;autoIncrement"`
    Email     string    `orm:"column:email_address"`
    CreatedAt time.Time
    Posts     []Post    `orm:"hasMany:UserID"`
}

sqlDB, _ := sql.Open("sqlite3", "app.db")
db := orm.Open(sqlDB, orm.SQLite())

users, err := db.Model[User]().
    Where(orm.Eq("active", true)).
    With("Posts").
    OrderBy(orm.Desc("created_at")).
    Limit(20).
    All(ctx)
```

## Queries

`Model[T]` and `Raw[T]` are generic methods on both `DB` and `Tx`, so a
transaction is a drop-in replacement for the connection:

```go
db.Model[User]()                    tx.Model[User]()
db.Raw[Report](query, args...)      tx.Raw[Report](query, args...)
```

Builder methods return copies, so a partially built query can be shared and
branched without aliasing.

`DB.Transaction` is the normal way to run a transaction: it commits, rolls back
and recovers from panics. `DB.Begin` (and `DB.BeginTx` for isolation levels)
returns a `*Tx` the caller must finish itself, for code that has to hand the
handle around.

| Category | Methods |
|---|---|
| Filter | `Where`, `Having`, `GroupBy`, `Distinct`, `Scope` |
| Shape | `Select`, `OrderBy`, `Limit`, `Offset`, `Table` |
| Join | `Join[U]`, `LeftJoin[U]`, `JoinTable`, `LeftJoinTable` |
| Read | `All`, `First`, `Find`, `Count`, `Exists`, `Paginate` |
| Transaction | `DB.Transaction`, `DB.Begin`, `DB.BeginTx` |
| Typed read | `Pluck[V]`, `As[R]`, `Sum[V]`, `Avg[V]`, `Min[V]`, `Max[V]` |
| Write | `Create`, `CreateBatch`, `Update`, `Delete`, `UpdateAll`, `DeleteAll` |
| Upsert | `Upsert`, `InsertIgnore` |
| Soft delete | `WithTrashed`, `OnlyTrashed`, `Restore`, `ForceDelete`, `ForceDeleteAll` |
| Relations | `With`, and `db.Relation[P, C]` |

### Predicates

Two ways to build the same `Predicate`, producing byte-identical SQL:

```go
// String constructors — no code generation needed.
orm.Eq / Ne / Gt / Gte / Lt / Lte / In / NotIn / Like / NotLike
orm.IsNull / IsNotNull / Between / And / Or / Not / RawPredicate

// Typed descriptors — compile-time column and value checking.
var UserFields = struct {
    ID     orm.Field[User, int]
    Active orm.Field[User, bool]
}{
    ID:     orm.NewField[User, int]("id"),
    Active: orm.NewField[User, bool]("active"),
}

db.Model[User]().Where(UserFields.Active.Eq(true))
```

Descriptors are ordinary Go values, so they can be hand-written. `cmd/ormgen`
generates them:

```bash
go run github.com/lemmego/orm/cmd/ormgen ./internal/models
```

An application using [`ormconnector`](https://github.com/lemmego/ormconnector) gets the same generator
as an `orm:fields` CLI command. The generator logic lives in the importable
`ormgen` package, so `go/ast` is not pulled in by importing the ORM itself.

or, more usually, from a directive in the models package:

```go
//go:generate go run github.com/lemmego/orm/cmd/ormgen .
```

It writes `orm_fields.go` containing a `<Model>Fields` value and a
`<Model>Table` constant per model. By default it generates for exported structs
carrying an `orm` or `db` tag; `-type Name,Other` selects specific types and
`-all` includes every exported struct.

The generator uses the ORM's own naming and tag rules, so a generated
descriptor always resolves to the same column the runtime does. It honours
embedded structs, `orm:"-"`, the `db` tag fallback, and a literal
`TableName()` override, and it skips relation fields.

### Result-type changes

`Query[T]` is not locked to returning `[]T`:

```go
emails, err := db.Model[User]().Pluck[string](ctx, "email_address")
total, err  := db.Model[Order]().Where(...).Sum[int64](ctx, "cents")

rows, err := db.Model[User]().
    Join[Post](orm.RawPredicate(`"posts"."user_id" = "users"."id"`)).
    As[UserPostRow]().
    All(ctx)
```

## Relationships

`hasOne`, `hasMany` and `belongsTo` are declared in struct tags. The tag names
the *field* holding the foreign key, not the column:

```go
type User struct {
    ID    int    `orm:"primaryKey;autoIncrement"`
    Posts []Post `orm:"hasMany:UserID"`   // UserID lives on Post
}

type Post struct {
    ID       int       `orm:"primaryKey;autoIncrement"`
    UserID   int
    Author   *User     `orm:"belongsTo:UserID"`   // UserID lives on Post
    Comments []Comment `orm:"hasMany:PostID"`
}
```

`With` eager-loads, including nested paths, at a cost of one query per
relation level regardless of row count:

```go
users, err := db.Model[User]().With("Posts.Comments").All(ctx)
```

Association writes take the database handle as the receiver, keeping models
free of any connection:

```go
db.Relation[User, Post](&user, "Posts").Add(ctx, &post)     // insert or relink
db.Relation[User, Post](&user, "Posts").Remove(ctx, &post)  // clears the FK, never deletes
db.Relation[User, Post](&user, "Posts").Load(ctx)
db.Relation[User, Post](&user, "Posts").Query().Where(...)  // further filtering
```

### Many-to-many

Declare the join table; the pivot columns default to the conventional
`<singular model>_id` on each side and can be overridden with
`joinForeignKey:` and `joinTargetKey:`.

```go
type Article struct {
    ID   int   `orm:"primaryKey;autoIncrement"`
    Tags []Tag `orm:"many2many:article_tags"`   // article_id, tag_id
}
```

Eager loading costs one pivot query plus one target query per level. The
association methods mirror Laravel's, with key-based variants for when the
related rows are known by identifier rather than loaded:

```go
tags := db.Relation[Article, Tag](&article, "Tags")

tags.Attach(ctx, &go, &sql)   // idempotent: existing links are not duplicated
tags.Detach(ctx, &sql)        // Detach(ctx) with no arguments clears everything
tags.Sync(ctx, &go, &orm)     // make the set exactly this
tags.Toggle(ctx, &go)         // flip membership

tags.AttachKeys(ctx, 1, 2)    // and DetachKeys / SyncKeys / ToggleKeys
```

`Add` and `Remove` delegate to `Attach` and `Detach` on a many-to-many, so the
handle behaves sensibly whichever verb you reach for.

## Soft deletes

A model is soft-deletable when it has a nullable timestamp named `DeletedAt`,
or any nullable timestamp tagged `orm:"softDelete"`:

```go
type Article struct {
    ID        int        `orm:"primaryKey;autoIncrement"`
    DeletedAt *time.Time            // or sql.NullTime
}
```

The column must be `*time.Time` or `sql.NullTime`. A bare `time.Time` is
rejected, because its zero value still writes a real timestamp and
`deleted_at IS NULL` would then match nothing — quietly hiding every row.

Reads exclude trashed rows by default, and so do `Count`, `Exists`,
`Paginate`, `UpdateAll` and `DeleteAll`:

```go
db.Model[Article]().All(ctx)                  // visible rows only
db.Model[Article]().WithTrashed().All(ctx)    // everything
db.Model[Article]().OnlyTrashed().All(ctx)    // just the trashed

db.Model[Article]().Delete(ctx, &article)       // marks deleted_at
db.Model[Article]().Restore(ctx, &article)      // clears it
db.Model[Article]().ForceDelete(ctx, &article)  // really removes the row
```

`DeleteAll` soft-deletes. `ForceDeleteAll` removes rows, but still honours the
default scope — combine it with `WithTrashed()` to purge everything.

The soft-delete column is resolved once from the model and carried through
`As[R]`, so a projection cannot accidentally read trashed rows.

## Automatic timestamps

Fields named `CreatedAt` and `UpdatedAt` of a time type are maintained
automatically, as is any field tagged `autoCreateTime` or `autoUpdateTime`.
`CreatedAt` is only filled when unset, so an explicitly assigned value (or one
set by a `BeforeCreate` hook) survives; `UpdatedAt` is always refreshed on
update.

The type check is deliberate: a model carrying its own `CreatedAt int64` keeps
full manual control rather than having the ORM overwrite it.

Stamps are taken in **UTC**. A naive column (Postgres `TIMESTAMP`, MySQL
`DATETIME`) stores no offset, so writing local time and reading it back returns
a value shifted by the machine's zone offset. Normalising to UTC makes the
round trip exact whether or not the column is timezone-aware. Values you assign
yourself are stored as given.

## Pagination and scopes

```go
page, err := db.Model[Article]().OrderBy(orm.Desc("created_at")).Paginate(ctx, 2, 20)
// page.Items, page.Total, page.TotalPages, page.HasNext(), page.HasPrev()
```

`Total` counts the whole result set, ignoring any limit or offset already on
the query. Aggregates drop `ORDER BY`: it is meaningless for a count, and
Postgres rejects `SELECT COUNT(*) ... ORDER BY id` outright.

Scopes name a reusable filter and compose:

```go
func Popular() orm.Scope[Article] {
    return func(q *orm.Query[Article]) *orm.Query[Article] {
        return q.Where(orm.Gte("views", 1000))
    }
}

db.Model[Article]().Scope(Popular(), Recent()).All(ctx)
```

## Upserts

`Upsert` inserts, or updates the named columns when a row with the same
conflict columns already exists. With no update columns named, every mapped
column except the conflict columns and the generated key is overwritten.

```go
db.Model[User]().Upsert(ctx, &user, []string{"email"}, "name", "updated_at")
db.Model[User]().InsertIgnore(ctx, &user, "email")   // insert, or leave it alone
```

The three databases spell this differently and the dialect hides the
difference: `ON CONFLICT (...) DO UPDATE` on Postgres and SQLite,
`ON DUPLICATE KEY UPDATE` on MySQL. Two MySQL differences are worth knowing:
it keys the conflict off *any* unique index rather than the columns you name,
and it has no `DO NOTHING`, so `InsertIgnore` compiles to the conventional
self-assignment no-op.

A generated key is not written back by an upsert: the statement may update an
existing row rather than insert one, so there is no new key to report.

## Transactions and savepoints

```go
err := db.Transaction(ctx, func(tx *orm.Tx) error {
    if _, err := tx.Model[User]().Create(ctx, &user); err != nil {
        return err
    }
    if err := tx.Savepoint(ctx, "after_user"); err != nil {
        return err
    }
    if _, err := tx.Model[Post]().Create(ctx, &post); err != nil {
        return tx.RollbackTo(ctx, "after_user")   // keep the user, drop the post
    }
    return tx.ReleaseSavepoint(ctx, "after_user")
})
```

A transaction is not bound to one entity type, so a single unit of work spans
as many models as it needs. `AfterCommit` registers callbacks that run only
once the commit succeeds and cannot roll it back.

Every registered callback runs, and their errors are joined rather than the
first one returned — stopping at the first would silently skip work that was
registered precisely because it has to happen after the commit. A failure
there is wrapped in `ErrAfterCommit`, so a caller can tell it from a commit
failure:

```go
if err := tx.CommitContext(ctx); err != nil {
    if errors.Is(err, orm.ErrAfterCommit) {
        // The transaction committed. Repair or report the callback —
        // never replay the unit of work, which would apply it twice.
    }
}
```

Savepoint names are written into the SQL text rather than bound as parameters,
so they are validated as plain identifiers; anything else returns
`ErrInvalidIdentifier`. `CheckIdentifier` is exported for callers that build
names themselves.

## Multi-tenancy

A model joins one tenant's rows by tagging exactly one field:

```go
type Invoice struct {
    TenantID uint64 `db:"tenant_id" orm:"tenantKey"`
    ID       uint64 `db:"id" orm:"primaryKey;autoIncrement"`
    Total    int64  `db:"total_cents"`
}
```

The binding rides on the **handle**, not the context, because a `Query[T]`
carries no context — the context arrives at execution, after the predicates
have been decided. So a tenant-scoped model is read and written through a
tenant transaction:

```go
err := db.TenantTx(ctx, tenantID, func(tx *orm.Tx) error {
    invoices, err := tx.Model[Invoice]().All(ctx)   // filtered
    _, err = tx.Model[Invoice]().Create(ctx, &Invoice{Total: 500}) // stamped
    return err
})
```

**It fails closed.** `db.Model[Invoice]()` on an unbound handle sets the error
at construction, so every terminal method reports it rather than one of them
quietly returning every tenant's rows. An insert naming a different tenant is
`ErrTenantMismatch`, not a silently-filtered write.

The predicate reaches reads, counts, pagination, bulk mutations,
single-row writes, soft deletes and eager loads. Two things it does not reach,
on purpose: **`Raw` SQL**, because there is nowhere to inject a predicate into
a hand-written statement, and **many-to-many pivot reads**, because a pivot is
a bare table with no model and therefore no declared tenant column — guessing
the name is the naming convention the metadata parser refuses.

Which is why this layer is the one that gives you a good error message rather
than the one that makes the guarantee.

### Telling the database too

The guarantee wants the database enforcing the same value, which on PostgreSQL
means row-level security reading a transaction-local setting. This package
does not know what that setting is called or whether a policy reads it — but
it does know what statement the dialect needs:

```go
db := orm.Open(sqlDB, orm.Postgres(),
    orm.WithTenantEnter(orm.SetTenantSetting("acme.tenant_id")))
```

`SetTenantSetting` runs once as each tenant transaction opens, before any
statement in it, and asks the dialect for the SQL. On PostgreSQL that is
`set_config(name, value, true)` — **transaction-local**, so it resets at
commit or rollback and cannot reach the next user of a pooled connection,
which is what makes the arrangement safe behind a pooler in transaction mode.
A session-level `SET` would leak the tenant, and would look perfect in
development under one connection and one user.

`TenantEnterDialect` is **optional and type-asserted**: PostgreSQL implements
it, MySQL and SQLite do not, and their not implementing it is an answer rather
than a gap. Ask before you promise:

```go
if _, ok := orm.TenantEnterStatement(db.Dialect()); !ok {
    return fmt.Errorf("this database cannot enforce tenant isolation: %s",
        db.Dialect().Name())
}
```

Without that check, an application that asked for two isolation layers gets
one and nothing says so. `SetTenantSetting` itself returns
`ErrTenantSettingUnsupported` naming the dialect the first time a tenant
transaction opens, and rolls it back rather than running statements against a
transaction the database does not consider scoped.

`WithTenantEnter` remains the escape hatch for a database whose binding this
package has never heard of.

## Observing writes

`db.Observe` registers a process-wide observer, called after a successful
write. It is how a search index, a cache or a feed stays in step with the
database without every call site remembering to tell it.

```go
db.Observe(func(ctx context.Context, change orm.Change) {
    post, ok := change.Entity.(*Post)
    if !ok {
        return
    }
    switch change.Op {
    case orm.ChangeDelete:
        _ = search.Forget(ctx, engine, post)
    default:
        _ = search.Sync(ctx, engine, post)
    }
})
```

**Inside a transaction it fires after the commit**, and that is the whole
value. GORM's callbacks and Bun's query hooks both fire *inside* the
transaction: a change announced there is announced before the row is visible
to anything else, so a subscriber that refetches finds nothing — and if the
transaction then rolls back, something has been told about a row that does not
exist. A change raised through `db.Model[T]()` fires immediately; one raised
through `tx.Model[T]()` is held until `CommitContext` succeeds and discarded
by `Rollback`. Nothing the caller writes makes the difference; the handle
does.

`Observe` is a method on `*DB` and not a package-level registry, which is not
stylistic: a global registry fires for every handle in the process and cannot
be cleaned up between test cases, so one case's observer satisfies the next
case's assertion.

What an observer must not be:

- **The place a counter is incremented.** Two concurrent observers both read
  41 and both write 42, and the lost vote is silent and permanent. That is
  what a database trigger or `SET c = c + 1` is for.
- **Somewhere to do slow work.** An observer on `*DB` runs on the goroutine
  that did the write, so anything expensive is paid by the request. Hand off
  to a queue.

A panicking observer does not take the write down — each runs under
`recover()` — and the panic is logged rather than swallowed.

A soft delete is reported as `ChangeDelete` even though the statement is an
`UPDATE`: what it means downstream is that the row is gone, and an observer
told "updated" would reindex a row it should remove. `Restore` is reported as
`ChangeUpdate`.

**`UpdateAll` and `DeleteAll` observe nothing.** There is no entity to
announce, and the matched set is unknown without a `RETURNING` or a prior
`SELECT` that changes the statement's cost and its lock footprint. A loud
nothing beats a surprise query, so the limitation is pinned by a test: a bulk
mutation is reconciled by a reimport, not by this.

## Query logging

Install a logger and every statement the ORM runs is reported — including the
extra queries eager loading issues, which are usually the ones you want to see:

```go
db := orm.Open(sqlDB, orm.Postgres(), orm.WithSlogLogger(slog.Default()))

// or handle the events yourself
db = orm.Open(sqlDB, orm.Postgres(), orm.WithLogger(
    func(ctx context.Context, e orm.QueryEvent) {
        if e.Duration > 100*time.Millisecond {
            log.Printf("slow: %s", e)
        }
    }))
```

`QueryEvent` carries the SQL, bound arguments, duration, error, and rows
affected — the last is `-1` for a read, since rows are counted as they are
scanned, long after the statement returns.

The logger is called synchronously on the query path, so hand off to a channel
if the work is expensive. With no logger installed nothing is wrapped and the
uninstrumented path costs nothing.

## Mapping

- Tags: `orm:"column:name"`, `primaryKey`, `autoIncrement`, `-`, and the
  relation forms above. A `db:"..."` tag is honoured as a fallback, with
  modifiers such as `,omitempty` stripped, so CLI-generated models map without
  edits.
- Table names are `strcase.ToSnake` plus `go-pluralize`, matching `/cli` and
  `/migration`. Implement `TableName() string` to override.
- Fields that cannot round-trip through one column (nested structs, slices of
  structs) are skipped rather than mapped.
- `SELECT` names its columns explicitly, so a column present in the table but
  absent from the struct is simply not read.
- `sql.Scanner` and `driver.Valuer` are honoured. `NULL` read into a
  non-pointer field yields the zero value instead of failing the scan.
- Raw queries tolerate columns the destination does not declare; model queries
  do not, so a mistyped `Select` is reported rather than silently dropped.

## Writes, hooks and errors

Generated keys are written back onto the entity after insert, via `RETURNING`
where the dialect supports it and `LastInsertId` where it does not.

`UpdateAll` and `DeleteAll` refuse to run without a predicate, returning
`ErrUnsafeMutation`.

Lifecycle order:

```text
BeforeCreate/BeforeUpdate/BeforeDelete
SQL statement
AfterCreate/AfterUpdate/AfterDelete
transaction commit
AfterCommit callbacks
```

An after-statement hook error is returned to the caller, and rolls back when
inside `DB.Transaction`. After-commit callbacks run only once the commit has
succeeded and cannot roll back.

Driver errors are normalised into `*ConstraintError` carrying a kind, and the
table and columns where the driver reports them:

```go
if orm.IsUniqueViolation(err) { ... }

var constraint *orm.ConstraintError
if errors.As(err, &constraint) {
    log.Println(constraint.Kind, constraint.Table, constraint.Columns)
}
```

Sentinels: `ErrNotFound`, `ErrClosed`, `ErrUnsafeMutation`, `ErrUnsupported`,
`ErrNoRowsAffected`, `ErrConstraint`.

## GPA

The [`gpaorm`](https://github.com/lemmego/gpaorm) module implements the GPA persistence contracts on
top of this ORM, so an application written against `gpa.Repository[T]` can run
on it. It is entirely optional: the ORM never imports GPA, and everything above
works without it.

Where the two overlap, the ORM owns the behaviour and the adapter delegates —
savepoints, for instance, are implemented on `orm.Tx` and `gpaorm` calls them,
rather than carrying a second copy of the SQL.

## Composite primary keys

A model may declare several `primaryKey` columns. `Update` and `Delete` then
address the row by all of them, and `Find` takes one value per column in
declaration order:

```go
type Enrollment struct {
    StudentID int `orm:"primaryKey"`
    CourseID  int `orm:"primaryKey"`
    Grade     string
}

enrollment, err := db.Model[Enrollment]().Find(ctx, studentID, courseID)
```

`ModelInfo.PrimaryKeys` holds them all; `ModelInfo.PrimaryKey` is the
single-key convenience and is nil for a composite key, so code cannot use it by
accident. Relations join on one column, so a composite-key model needs
`references:` to name which — otherwise eager loading reports the problem
rather than guessing.

## Not implemented yet

Row locking and subqueries; the GPA adapter reports both as unsupported rather
than ignoring them. Keyset pagination, an atomic increment in `UpdateAll`
(`col = col + 1` is inexpressible, since every value is bound as a
placeholder), and ordered or limited eager loading.

`Observe` does not report the *old* value of a changed row, so "what became
false" is not answerable from it. An observer that needs that reads the row
before the write.

## Verification

```bash
cd orm
go build ./... && go vet ./... && go test ./... && go test -race ./...
GOWORK=off go test ./...   # validate as a published module
```
