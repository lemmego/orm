package orm

import (
	"context"
	"strings"
	"testing"
)

// Enrollment is keyed on two columns, the shape a join table usually takes.
type Enrollment struct {
	StudentID int `orm:"primaryKey"`
	CourseID  int `orm:"primaryKey"`
	Grade     string
}

// Seat has a composite key and a hasMany relation. Eager loading has to pick
// one column to join on, which a composite key cannot supply.
type Seat struct {
	RowNumber int          `orm:"primaryKey"`
	Position  int          `orm:"primaryKey"`
	Tickets   []SeatTicket `orm:"hasMany:SeatRow"`
}

type SeatTicket struct {
	ID      int `orm:"primaryKey;autoIncrement"`
	SeatRow int
}

const compositeSchema = `
CREATE TABLE enrollments (
    student_id INTEGER,
    course_id INTEGER,
    grade TEXT,
    PRIMARY KEY (student_id, course_id)
);
CREATE TABLE seats (row_number INTEGER, position INTEGER, PRIMARY KEY (row_number, position));
CREATE TABLE seat_tickets (id INTEGER PRIMARY KEY AUTOINCREMENT, seat_row INTEGER);
`

func openCompositeDB(t *testing.T) *DB {
	t.Helper()
	db := openTestDB(t)
	if _, err := db.SQLDB().Exec(compositeSchema); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestCompositeKeyMetadata(t *testing.T) {
	info := mustInfo[Enrollment](t)

	if len(info.PrimaryKeys) != 2 {
		t.Fatalf("expected 2 primary keys, got %d", len(info.PrimaryKeys))
	}
	if !info.HasCompositeKey() {
		t.Fatal("expected HasCompositeKey to report true")
	}
	// The single-key convenience must be nil so callers cannot use it by
	// accident.
	if info.PrimaryKey != nil {
		t.Fatal("PrimaryKey must be nil for a composite key")
	}
	if info.PrimaryKeys[0].Column != "student_id" || info.PrimaryKeys[1].Column != "course_id" {
		t.Fatalf("keys are out of declaration order: %#v", info.PrimaryKeys)
	}

	// A single-key model keeps both views populated.
	single := mustInfo[User](t)
	if single.HasCompositeKey() || single.PrimaryKey == nil || len(single.PrimaryKeys) != 1 {
		t.Fatalf("unexpected single-key metadata: %#v", single.PrimaryKeys)
	}
}

func TestCompositeKeyUpdateAndDeleteSQL(t *testing.T) {
	db := testDB(Postgres())
	enrollment := &Enrollment{StudentID: 7, CourseID: 9, Grade: "A"}

	update, args, err := db.Model[Enrollment]().UpdateSQL(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	want := `UPDATE "enrollments" SET "grade" = $1 WHERE "student_id" = $2 AND "course_id" = $3`
	if update != want {
		t.Fatalf("got  %s\nwant %s", update, want)
	}
	if len(args) != 3 || args[1] != 7 || args[2] != 9 {
		t.Fatalf("unexpected args: %#v", args)
	}

	remove, removeArgs, err := db.Model[Enrollment]().DeleteSQL(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	wantDelete := `DELETE FROM "enrollments" WHERE "student_id" = $1 AND "course_id" = $2`
	if remove != wantDelete {
		t.Fatalf("got  %s\nwant %s", remove, wantDelete)
	}
	if len(removeArgs) != 2 {
		t.Fatalf("unexpected args: %#v", removeArgs)
	}
}

func TestCompositeKeyRoundTrip(t *testing.T) {
	db, ctx := openCompositeDB(t), context.Background()

	first := &Enrollment{StudentID: 1, CourseID: 10, Grade: "B"}
	second := &Enrollment{StudentID: 1, CourseID: 20, Grade: "C"}
	for _, enrollment := range []*Enrollment{first, second} {
		if _, err := db.Model[Enrollment]().Create(ctx, enrollment); err != nil {
			t.Fatal(err)
		}
	}

	found, err := db.Model[Enrollment]().Find(ctx, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if found.Grade != "C" {
		t.Fatalf("Find addressed the wrong row: %#v", found)
	}

	first.Grade = "A"
	if _, err := db.Model[Enrollment]().Update(ctx, first); err != nil {
		t.Fatal(err)
	}
	// Updating one row must not touch the row sharing a key column.
	untouched, err := db.Model[Enrollment]().Find(ctx, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if untouched.Grade != "C" {
		t.Fatalf("the sibling row was modified: %#v", untouched)
	}

	if _, err := db.Model[Enrollment]().Delete(ctx, first); err != nil {
		t.Fatal(err)
	}
	count, err := db.Model[Enrollment]().Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one row to be deleted, %d remain", count)
	}
}

func TestFindRejectsWrongKeyCount(t *testing.T) {
	db, ctx := openCompositeDB(t), context.Background()

	_, err := db.Model[Enrollment]().Find(ctx, 1)
	if err == nil {
		t.Fatal("expected a composite key to require both values")
	}
	if !strings.Contains(err.Error(), "keyed on 2 columns") {
		t.Fatalf("the error should say how many keys are needed: %v", err)
	}
}

// Relations join on one column, so a composite-key model must say so rather
// than failing obscurely.
func TestCompositeKeyRelationReportsClearly(t *testing.T) {
	db, ctx := openCompositeDB(t), context.Background()

	if _, err := db.Model[Seat]().Create(ctx, &Seat{RowNumber: 1, Position: 2}); err != nil {
		t.Fatal(err)
	}

	_, err := db.Model[Seat]().With("Tickets").All(ctx)
	if err == nil {
		t.Fatal("expected relation loading to be refused for a composite key")
	}
	if !strings.Contains(err.Error(), "single primary key") {
		t.Fatalf("unexpected error: %v", err)
	}
}
