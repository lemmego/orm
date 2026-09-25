# ORM Architecture

## Dependency direction

```text
orm -> database/sql (+ a small inflector)
gpaorm -> orm + gpa
application -> orm or gpaorm
```

The ORM must not import GPA, GORM, or Bun. GPA remains a provider-neutral
abstraction; the future GPA adapter will translate GPA operations into ORM
operations.

The one concession to dependencies is `gertd/go-pluralize` and
`iancoleman/strcase`. Table and column naming has to agree exactly with what
`/cli` and `/migration` generate, and they use these packages. A hand-rolled
inflector previously turned `Category` into `categorys` while the generated
migration created `categories`, which broke silently at runtime. Sharing the
inflector removes a whole class of bug; `TableName()` remains the override.

## Generic methods

Go 1.27 permits methods to declare their own type parameters on concrete
types. An earlier version of this document claimed the opposite and shaped the
API around the supposed restriction; that was wrong. Query entry points are
receiver-oriented:

```go
users, err := db.Model[User]().
    Where(UserFields.Active.Eq(true)).
    All(ctx)

err := db.Transaction(ctx, func(tx *orm.Tx) error {
    _, err := tx.Model[User]().Where(orm.Eq("id", userID)).UpdateAll(ctx, ...)
    return err
})
```

The real restriction is that **a generic method can never satisfy an
interface** — interface methods may not declare type parameters
(https://go.dev/blog/generic-methods). Three consequences bind this codebase:

1. `DB` and `Tx` are concrete types with duplicated generic entry points.
   There is deliberately no `Querier` interface over them; one carrying
   `Model[T]()` would not compile.
2. `Dialect` and the hook interfaces must stay generic-method-free.
3. The `gpaorm` adapter satisfies `gpa.Repository[T]` with a concrete
   generic *type*, never a generic method.

Generic methods earn their place where the result type changes mid-chain,
which package-level functions cannot express fluently:

```go
emails, err := db.Model[User]().Where(...).Pluck[string](ctx, "email_address")
report, err := db.Model[User]().Join[Post](on).As[UserPostRow]().All(ctx)
total, err := db.Model[Order]().Where(...).Sum[int64](ctx, "cents")
```

## Code generation

The `ormgen` package emits the typed descriptors, and `cmd/ormgen` is its
command line. It is a separate package so that importing the ORM does not drag
`go/ast` into an application binary. It calls the ORM's own naming and tag
rules — `ColumnName`, `TableNameFor`, `ParseTag` in `naming.go` — rather than
reimplementing them, because a second, slightly different copy of those rules
is precisely what let the runtime look for `categorys` while the migration
created `categories`. `internal/genfixture` holds generated output that is
checked for drift against both the generator and the runtime metadata on every
test run.

Generation is optional. The ORM is fully usable without ever running it, which
is why the string constructors are the core rather than the veneer.

## Query API

Predicates are the single currency of the DSL. String constructors and typed
field descriptors both build a `Predicate`, so there is one `Where` signature
and one SQL compiler:

```go
db.Model[User]().Where(orm.Eq("active", true))        // no codegen required
db.Model[User]().Where(UserFields.Active.Eq(true))    // compile-time checked
```

The string form is the core rather than the veneer, for three reasons: the GPA
adapter receives column names as runtime strings and could not target a
descriptor-only API; the ORM stays usable before any generator exists; and it
matches the framework's Laravel-shaped ergonomics.

Predicates stay structured until compile time because identifier quoting and
placeholder numbering both depend on the dialect, which is unknown at
construction.

## Relationships

Relationships are declared with struct tags and loaded explicitly:

```go
type User struct {
    ID    int    `orm:"primaryKey;autoIncrement"`
    Posts []Post `orm:"hasMany:UserID"`
}

users, err := db.Model[User]().With("Posts.Comments").All(ctx)
```

Eager loading issues **one query per relation level**, using batched
`WHERE fk IN (...)` lookups rather than joins. Joins would multiply rows and
complicate the projection; batched lookups keep the cost independent of row
count. Deeper levels load before children are attached to their parents,
because attachment copies struct values.

Many-to-many joins through a pivot table declared as
`orm:"many2many:article_tags"`, with key columns defaulting to the
conventional `<singular model>_id`. Loading one costs two queries per level:
the pivot rows, then the targets.

Soft deletes require a nullable timestamp column. A bare `time.Time` is
rejected rather than accepted, because its zero value writes a real timestamp
and `deleted_at IS NULL` would then match no row at all — the failure mode is
an empty table rather than an error, so it is caught at metadata time.

Models stay plain structs. Association writes take the database handle as the
receiver rather than hanging off the model:

```go
db.Relation[User, Post](&user, "Posts").Add(ctx, &post)
```

A model that carries a live connection cannot be safely serialised, copied
between goroutines, or constructed in a test without a database, so
`user.Posts().Add(...)` is deliberately not offered.

## Milestone order

1. SQL execution, dialects, expressions, and immutable query state. (done)
2. Metadata registry and row mapping. (done)
3. CRUD and typed results. (done)
4. Hooks, normalized errors, and transaction callbacks. (done)
5. Explicit eager-loaded relationships. (done)
6. Many-to-many operations, scopes, soft deletes, and pagination. (done)
7. GPA adapter and generated descriptors. (done)

All seven milestones are implemented. The GPA adapter lives in the separate
`gpaorm` module so that this one keeps its narrow dependency set, and
`ormconnector` wires the ORM into the application lifecycle. The descriptor
generator is the `ormgen` package, with `cmd/ormgen` as its command line.
