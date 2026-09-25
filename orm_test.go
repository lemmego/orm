package orm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type User struct {
	ID        int    `orm:"primaryKey;autoIncrement"`
	Email     string `orm:"column:email_address"`
	CreatedAt int64
	Ignored   string `orm:"-"`
	Posts     []Post `orm:"hasMany:UserID"`
}

type Post struct {
	ID       int `orm:"primaryKey;autoIncrement"`
	UserID   int
	Title    string
	Author   *User     `orm:"belongsTo:UserID"`
	Comments []Comment `orm:"hasMany:PostID"`
}

type Comment struct {
	ID     int `orm:"primaryKey;autoIncrement"`
	PostID int
	Body   string
}

// Category exercises pluralisation that a naive "name + s" rule gets wrong.
type Category struct {
	ID   int `orm:"primaryKey;autoIncrement"`
	Name string
}

// Person exercises an irregular plural.
type Person struct {
	ID   int `orm:"primaryKey;autoIncrement"`
	Name string
}

// Account overrides its table name.
type Account struct {
	ID   int `orm:"primaryKey;autoIncrement"`
	Name string
}

func (Account) TableName() string { return "billing_accounts" }

// LegacyUser uses db tags, as the CLI's model generator emits today.
type LegacyUser struct {
	ID    uint64 `db:"id,omitempty"`
	Email string `db:"email"`
	Skip  string `db:"-"`
}

// UserEmail is a projection target for As.
type UserEmail struct {
	Email string `orm:"column:email_address"`
}

var (
	userActive = NewField[User, bool]("active")
	userID     = NewField[User, int]("id")
)

func testDB(dialect Dialect) *DB { return Open(nil, dialect) }

func TestSelectNamesColumnsInsteadOfStar(t *testing.T) {
	query, _ := testDB(Postgres()).Model[User]().SQL()
	want := `SELECT "id", "email_address", "created_at" FROM "users"`
	if query != want {
		t.Fatalf("got  %s\nwant %s", query, want)
	}
}

func TestQueryIsImmutable(t *testing.T) {
	base := testDB(Postgres()).Model[User]().Where(RawPredicate(`"active" = ?`, true))
	limited := base.Limit(10)

	query, args := limited.SQL()
	want := `SELECT "id", "email_address", "created_at" FROM "users" WHERE "active" = $1 LIMIT $2`
	if query != want {
		t.Fatalf("got  %s\nwant %s", query, want)
	}
	if len(args) != 2 || args[1] != 10 {
		t.Fatalf("unexpected args: %#v", args)
	}
	if baseQuery, _ := base.SQL(); baseQuery != `SELECT "id", "email_address", "created_at" FROM "users" WHERE "active" = $1` {
		t.Fatalf("base query was mutated: %s", baseQuery)
	}
}

// The same chain must render correctly under each dialect, with placeholder
// numbering shared across WHERE, LIMIT and OFFSET.
func TestDialectMatrix(t *testing.T) {
	cases := []struct {
		dialect Dialect
		want    string
	}{
		{Postgres(), `SELECT "id", "email_address", "created_at" FROM "users" WHERE "active" = $1 AND "id" IN ($2, $3) ORDER BY "created_at" DESC LIMIT $4 OFFSET $5`},
		{MySQL(), "SELECT `id`, `email_address`, `created_at` FROM `users` WHERE `active` = ? AND `id` IN (?, ?) ORDER BY `created_at` DESC LIMIT ? OFFSET ?"},
		{SQLite(), `SELECT "id", "email_address", "created_at" FROM "users" WHERE "active" = ? AND "id" IN (?, ?) ORDER BY "created_at" DESC LIMIT ? OFFSET ?`},
	}
	for _, testCase := range cases {
		t.Run(testCase.dialect.Name(), func(t *testing.T) {
			query, args := testDB(testCase.dialect).Model[User]().
				Where(userActive.Eq(true)).
				Where(userID.In(1, 2)).
				OrderBy(Desc("created_at")).
				Limit(10).
				Offset(20).
				SQL()
			if query != testCase.want {
				t.Fatalf("got  %s\nwant %s", query, testCase.want)
			}
			if len(args) != 5 {
				t.Fatalf("unexpected args: %#v", args)
			}
		})
	}
}

// The two halves of the hybrid API must be indistinguishable downstream.
func TestStringAndTypedPredicatesAreEquivalent(t *testing.T) {
	db := testDB(Postgres())
	cases := []struct{ stringly, typed Predicate }{
		{Eq("active", true), userActive.Eq(true)},
		{Ne("active", true), userActive.Ne(true)},
		{Gt("id", 5), userID.Gt(5)},
		{Gte("id", 5), userID.Gte(5)},
		{Lt("id", 5), userID.Lt(5)},
		{Lte("id", 5), userID.Lte(5)},
		{In("id", 1, 2, 3), userID.In(1, 2, 3)},
		{NotIn("id", 1, 2), userID.NotIn(1, 2)},
		{Between("id", 1, 9), userID.Between(1, 9)},
		{IsNull("active"), userActive.IsNull()},
		{IsNotNull("active"), userActive.IsNotNull()},
	}
	for _, testCase := range cases {
		stringQuery, stringArgs := db.Model[User]().Where(testCase.stringly).SQL()
		typedQuery, typedArgs := db.Model[User]().Where(testCase.typed).SQL()
		if stringQuery != typedQuery {
			t.Fatalf("SQL diverged:\n  string %s\n  typed  %s", stringQuery, typedQuery)
		}
		if !reflect.DeepEqual(stringArgs, typedArgs) {
			t.Fatalf("args diverged for %s: %#v vs %#v", stringQuery, stringArgs, typedArgs)
		}
	}
}

func TestCompositePredicates(t *testing.T) {
	query, args := testDB(Postgres()).Model[User]().
		Where(Or(Eq("id", 1), Eq("id", 2)).And(Eq("active", true))).
		SQL()
	want := `SELECT "id", "email_address", "created_at" FROM "users" WHERE (("id" = $1) OR ("id" = $2)) AND ("active" = $3)`
	if query != want {
		t.Fatalf("got  %s\nwant %s", query, want)
	}
	if len(args) != 3 {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestEmptyInListMatchesNothing(t *testing.T) {
	query, args := testDB(SQLite()).Model[User]().Where(In("id")).SQL()
	if query != `SELECT "id", "email_address", "created_at" FROM "users" WHERE 1 = 0` {
		t.Fatalf("unexpected query: %s", query)
	}
	if len(args) != 0 {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestRawPredicatePlaceholdersRenumber(t *testing.T) {
	query, _ := testDB(Postgres()).Model[User]().
		Where(Eq("active", true)).
		Where(RawPredicate(`"id" between ? and ?`, 1, 9)).
		SQL()
	want := `SELECT "id", "email_address", "created_at" FROM "users" WHERE "active" = $1 AND "id" between $2 and $3`
	if query != want {
		t.Fatalf("got  %s\nwant %s", query, want)
	}
}

func TestGroupByAndHaving(t *testing.T) {
	query, args := testDB(Postgres()).Model[User]().
		Select("created_at").
		GroupBy("created_at").
		Having(Gt("COUNT(*)", 2)).
		SQL()
	want := `SELECT "created_at" FROM "users" GROUP BY "created_at" HAVING COUNT(*) > $1`
	if query != want {
		t.Fatalf("got  %s\nwant %s", query, want)
	}
	if len(args) != 1 || args[0] != 2 {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestJoinUsesTargetModelTable(t *testing.T) {
	query, _ := testDB(Postgres()).Model[User]().
		Join[Post](RawPredicate(`"posts"."user_id" = "users"."id"`)).
		SQL()
	want := `SELECT "users"."id", "users"."email_address", "users"."created_at" FROM "users" INNER JOIN "posts" ON "posts"."user_id" = "users"."id"`
	if query != want {
		t.Fatalf("got  %s\nwant %s", query, want)
	}
}

func TestPluralizationMatchesInflector(t *testing.T) {
	cases := map[string]string{
		"users": mustInfo[User](t).Table, "categories": mustInfo[Category](t).Table,
		"people": mustInfo[Person](t).Table, "comments": mustInfo[Comment](t).Table,
	}
	for want, got := range cases {
		if got != want {
			t.Fatalf("got table %q, want %q", got, want)
		}
	}
}

func TestTableNameOverride(t *testing.T) {
	if table := mustInfo[Account](t).Table; table != "billing_accounts" {
		t.Fatalf("unexpected table: %s", table)
	}
}

func TestDBTagFallback(t *testing.T) {
	info := mustInfo[LegacyUser](t)
	if len(info.Fields) != 2 {
		t.Fatalf("unexpected fields: %#v", info.Fields)
	}
	if _, ok := info.ByColumn["id"]; !ok {
		t.Fatal("expected id column from db tag with omitempty stripped")
	}
	if _, ok := info.ByColumn["email"]; !ok {
		t.Fatal("expected email column")
	}
}

func TestRelationFieldsAreNotColumns(t *testing.T) {
	info := mustInfo[User](t)
	for _, field := range info.Fields {
		if field.Name == "Posts" {
			t.Fatal("relation field was mapped as a column")
		}
	}
	relation, ok := info.ByRelation["Posts"]
	if !ok {
		t.Fatal("expected Posts relation")
	}
	if relation.Kind != HasMany || relation.ForeignKeyField != "UserID" {
		t.Fatalf("unexpected relation: %#v", relation)
	}
	if relation.Target != reflect.TypeFor[Post]() {
		t.Fatalf("unexpected target: %s", relation.Target)
	}
}

func TestModelMetadata(t *testing.T) {
	info := mustInfo[User](t)
	if len(info.Fields) != 3 {
		t.Fatalf("unexpected fields: %#v", info.Fields)
	}
	if info.PrimaryKey == nil || info.PrimaryKey.Column != "id" {
		t.Fatalf("unexpected primary key: %#v", info.PrimaryKey)
	}
	if info.ByColumn["email_address"].Name != "Email" {
		t.Fatal("expected tagged email column")
	}
}

func TestModelMetadataRejectsScalars(t *testing.T) {
	if _, err := ModelInfoFor[int](); err == nil {
		t.Fatal("expected scalar metadata error")
	}
}

func TestInsertSQLOmitsZeroAutoIncrementKey(t *testing.T) {
	query, args, err := testDB(Postgres()).Model[User]().InsertSQL(&User{Email: "user@example.com", CreatedAt: 123})
	if err != nil {
		t.Fatal(err)
	}
	if query != `INSERT INTO "users" ("email_address", "created_at") VALUES ($1, $2)` {
		t.Fatalf("unexpected insert: %s", query)
	}
	if len(args) != 2 || args[0] != "user@example.com" || args[1] != int64(123) {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestUpdateAndDeleteSQLUsePrimaryKey(t *testing.T) {
	db := testDB(Postgres())
	user := &User{ID: 7, Email: "updated@example.com", CreatedAt: 456}

	update, updateArgs, err := db.Model[User]().UpdateSQL(user)
	if err != nil {
		t.Fatal(err)
	}
	if update != `UPDATE "users" SET "email_address" = $1, "created_at" = $2 WHERE "id" = $3` {
		t.Fatalf("unexpected update: %s", update)
	}
	if len(updateArgs) != 3 || updateArgs[2] != 7 {
		t.Fatalf("unexpected update args: %#v", updateArgs)
	}

	remove, removeArgs, err := db.Model[User]().DeleteSQL(user)
	if err != nil {
		t.Fatal(err)
	}
	if remove != `DELETE FROM "users" WHERE "id" = $1` || len(removeArgs) != 1 || removeArgs[0] != 7 {
		t.Fatalf("unexpected delete: %s %#v", remove, removeArgs)
	}
}

func TestUpdateSQLSupportsColumnSubset(t *testing.T) {
	query, args, err := testDB(Postgres()).Model[User]().
		UpdateSQL(&User{ID: 7, Email: "a@b.c", CreatedAt: 9}, "email_address")
	if err != nil {
		t.Fatal(err)
	}
	if query != `UPDATE "users" SET "email_address" = $1 WHERE "id" = $2` {
		t.Fatalf("unexpected update: %s", query)
	}
	if len(args) != 2 {
		t.Fatalf("unexpected args: %#v", args)
	}
}

// An unfiltered bulk mutation is refused rather than silently rewriting the
// whole table.
func TestBulkMutationsRequireAPredicate(t *testing.T) {
	db := testDB(SQLite())
	if _, err := db.Model[User]().UpdateAll(context.Background(), map[string]any{"email_address": "x"}); !errors.Is(err, ErrUnsafeMutation) {
		t.Fatalf("expected ErrUnsafeMutation, got %v", err)
	}
	if _, err := db.Model[User]().DeleteAll(context.Background()); !errors.Is(err, ErrUnsafeMutation) {
		t.Fatalf("expected ErrUnsafeMutation, got %v", err)
	}
}

func TestUnsupportedFeatureMatchesSentinel(t *testing.T) {
	if !errors.Is(&UnsupportedFeatureError{Dialect: "sqlite", Feature: "returning"}, ErrUnsupported) {
		t.Fatal("expected unsupported feature to match ErrUnsupported")
	}
}

type testResult struct{ rows int64 }

func (r testResult) LastInsertId() (int64, error) { return 0, nil }
func (r testResult) RowsAffected() (int64, error) { return r.rows, nil }

func TestCheckRowsAffected(t *testing.T) {
	if err := CheckRowsAffected(testResult{}); err != ErrNoRowsAffected {
		t.Fatalf("expected no-rows error, got %v", err)
	}
	if err := CheckRowsAffected(testResult{rows: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestDialectErrorClassification(t *testing.T) {
	cases := []struct {
		dialect Dialect
		err     error
		kind    ConstraintKind
	}{
		{SQLite(), errors.New("UNIQUE constraint failed: users.email_address"), ConstraintUnique},
		{SQLite(), errors.New("NOT NULL constraint failed: users.email_address"), ConstraintNotNull},
		{SQLite(), errors.New("FOREIGN KEY constraint failed"), ConstraintForeignKey},
		{Postgres(), errors.New(`duplicate key value violates unique constraint "users_email_key"`), ConstraintUnique},
		{Postgres(), errors.New(`insert violates foreign key constraint "posts_user_id_fkey"`), ConstraintForeignKey},
		{MySQL(), errors.New("Error 1062 (23000): Duplicate entry 'a' for key 'users.email'"), ConstraintUnique},
	}
	for _, testCase := range cases {
		classified := testCase.dialect.ClassifyError(testCase.err)
		var constraint *ConstraintError
		if !errors.As(classified, &constraint) {
			t.Fatalf("%s: expected ConstraintError, got %v", testCase.dialect.Name(), classified)
		}
		if constraint.Kind != testCase.kind {
			t.Fatalf("%s: got kind %s, want %s", testCase.dialect.Name(), constraint.Kind, testCase.kind)
		}
		if !errors.Is(classified, ErrConstraint) {
			t.Fatal("expected ErrConstraint sentinel match")
		}
		if !errors.Is(classified, testCase.err) {
			t.Fatal("expected the driver error to remain unwrappable")
		}
	}
}

func TestUnrecognisedErrorsPassThrough(t *testing.T) {
	original := errors.New("connection reset")
	if got := SQLite().ClassifyError(original); got != original {
		t.Fatalf("expected passthrough, got %v", got)
	}
}

func mustInfo[T any](t *testing.T) *ModelInfo {
	t.Helper()
	info, err := ModelInfoFor[T]()
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// Postgres rejects "SELECT COUNT(*) ... ORDER BY id" with "column must appear
// in the GROUP BY clause or be used in an aggregate function". Paginate counts
// the query it is about to page over, so an ordered query must drop its
// ordering for the count.
func TestAggregatesDropOrderBy(t *testing.T) {
	ordered := testDB(Postgres()).Model[User]().
		Where(Eq("active", true)).
		OrderBy(Desc("created_at"))

	countSQL, _, err := ordered.unordered().buildSelect("COUNT(*)")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(countSQL, "ORDER BY") {
		t.Fatalf("an aggregate must not carry ORDER BY: %s", countSQL)
	}
	want := `SELECT COUNT(*) FROM "users" WHERE "active" = $1`
	if countSQL != want {
		t.Fatalf("got  %s\nwant %s", countSQL, want)
	}

	// The original query keeps its ordering.
	selectSQL, _ := ordered.SQL()
	if !strings.Contains(selectSQL, "ORDER BY") {
		t.Fatalf("the source query lost its ordering: %s", selectSQL)
	}
}

// Automatic timestamps are stored in UTC. A naive column keeps no offset, so
// writing local time and reading it back shifts the value by the machine's
// zone offset.
func TestAutomaticTimestampsAreUTC(t *testing.T) {
	info := mustInfo[Article](t)
	article := &Article{Title: "utc"}
	if err := applyCreateTimestamps(info, article); err != nil {
		t.Fatal(err)
	}
	if name, _ := article.CreatedAt.Zone(); name != "UTC" {
		t.Fatalf("CreatedAt was stamped in %s, want UTC", name)
	}
	if name, _ := article.UpdatedAt.Zone(); name != "UTC" {
		t.Fatalf("UpdatedAt was stamped in %s, want UTC", name)
	}

	if err := applyUpdateTimestamp(info, article); err != nil {
		t.Fatal(err)
	}
	if name, _ := article.UpdatedAt.Zone(); name != "UTC" {
		t.Fatalf("UpdatedAt refreshed in %s, want UTC", name)
	}
}
