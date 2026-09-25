package orm

import (
	"fmt"
	"strings"
)

// Dialect controls SQL syntax and error semantics that differ between
// supported databases.
//
// Dialect is an interface, so it must never gain methods with their own type
// parameters: Go 1.27 generic methods exist on concrete types only and cannot
// participate in interface satisfaction.
type Dialect interface {
	Name() string
	Quote(string) string
	Placeholder(int) string

	// SupportsReturning reports whether INSERT ... RETURNING is available.
	// Where it is not, generated keys come from LastInsertId instead.
	SupportsReturning() bool

	// ClassifyError maps a driver error onto a normalised ORM error such as
	// *ConstraintError. It returns the original error when unrecognised.
	ClassifyError(error) error

	// UpsertClause renders the suffix that turns an INSERT into an upsert.
	// conflict names the columns that identify an existing row; update names
	// the columns to overwrite, and being empty means "leave the row alone".
	UpsertClause(conflict, update []string) (string, error)
}

// conflictTargetClause renders the "ON CONFLICT (a, b)" target shared by the
// dialects that follow the SQL-standard spelling.
func conflictTargetClause(dialect Dialect, conflict, update []string) (string, error) {
	var target string
	if len(conflict) > 0 {
		quoted := make([]string, len(conflict))
		for i, column := range conflict {
			quoted[i] = dialect.Quote(column)
		}
		target = " (" + strings.Join(quoted, ", ") + ")"
	}

	if len(update) == 0 {
		return "ON CONFLICT" + target + " DO NOTHING", nil
	}
	if target == "" {
		return "", fmt.Errorf("orm: %s needs conflict columns to update on conflict", dialect.Name())
	}

	assignments := make([]string, len(update))
	for i, column := range update {
		quoted := dialect.Quote(column)
		assignments[i] = quoted + " = EXCLUDED." + quoted
	}
	return "ON CONFLICT" + target + " DO UPDATE SET " + strings.Join(assignments, ", "), nil
}

type postgresDialect struct{}

func (postgresDialect) Name() string             { return "postgres" }
func (postgresDialect) Quote(name string) string { return `"` + name + `"` }
func (postgresDialect) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }
func (postgresDialect) SupportsReturning() bool  { return true }

func (d postgresDialect) UpsertClause(conflict, update []string) (string, error) {
	return conflictTargetClause(d, conflict, update)
}

func (postgresDialect) ClassifyError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "violates unique constraint"),
		strings.Contains(message, "duplicate key value"):
		return &ConstraintError{Kind: ConstraintUnique, Constraint: quotedSegment(message), Cause: err}
	case strings.Contains(message, "violates foreign key constraint"):
		return &ConstraintError{Kind: ConstraintForeignKey, Constraint: quotedSegment(message), Cause: err}
	case strings.Contains(message, "violates not-null constraint"):
		return &ConstraintError{Kind: ConstraintNotNull, Columns: columnsFromQuoted(message), Cause: err}
	case strings.Contains(message, "violates check constraint"):
		return &ConstraintError{Kind: ConstraintCheck, Constraint: quotedSegment(message), Cause: err}
	}
	return err
}

type mysqlDialect struct{}

func (mysqlDialect) Name() string             { return "mysql" }
func (mysqlDialect) Quote(name string) string { return "`" + name + "`" }
func (mysqlDialect) Placeholder(int) string   { return "?" }
func (mysqlDialect) SupportsReturning() bool  { return false }

// UpsertClause uses MySQL's ON DUPLICATE KEY form.
//
// Two differences from the standard spelling are worth knowing: MySQL keys the
// conflict off *any* unique index rather than the columns you name, so the
// conflict argument only selects the no-op target; and VALUES() is deprecated
// from 8.0.20 but still accepted, which keeps this working on older servers.
func (d mysqlDialect) UpsertClause(conflict, update []string) (string, error) {
	if len(update) == 0 {
		// MySQL has no DO NOTHING. Assigning a column to itself is the
		// conventional no-op that still swallows the duplicate.
		anchor := ""
		if len(conflict) > 0 {
			anchor = d.Quote(conflict[0])
		}
		if anchor == "" {
			return "", fmt.Errorf("orm: mysql needs a column to anchor a do-nothing upsert")
		}
		return "ON DUPLICATE KEY UPDATE " + anchor + " = " + anchor, nil
	}

	assignments := make([]string, len(update))
	for i, column := range update {
		quoted := d.Quote(column)
		assignments[i] = quoted + " = VALUES(" + quoted + ")"
	}
	return "ON DUPLICATE KEY UPDATE " + strings.Join(assignments, ", "), nil
}

func (mysqlDialect) ClassifyError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "Error 1062"), strings.Contains(message, "Duplicate entry"):
		return &ConstraintError{Kind: ConstraintUnique, Constraint: quotedSegment(message), Cause: err}
	case strings.Contains(message, "Error 1452"), strings.Contains(message, "Error 1451"),
		strings.Contains(message, "foreign key constraint fails"):
		return &ConstraintError{Kind: ConstraintForeignKey, Constraint: quotedSegment(message), Cause: err}
	case strings.Contains(message, "Error 1048"), strings.Contains(message, "cannot be null"):
		return &ConstraintError{Kind: ConstraintNotNull, Columns: columnsFromQuoted(message), Cause: err}
	case strings.Contains(message, "Error 3819"), strings.Contains(message, "Check constraint"):
		return &ConstraintError{Kind: ConstraintCheck, Constraint: quotedSegment(message), Cause: err}
	}
	return err
}

type sqliteDialect struct{}

func (sqliteDialect) Name() string             { return "sqlite" }
func (sqliteDialect) Quote(name string) string { return `"` + name + `"` }
func (sqliteDialect) Placeholder(int) string   { return "?" }
func (sqliteDialect) SupportsReturning() bool  { return false }

func (d sqliteDialect) UpsertClause(conflict, update []string) (string, error) {
	return conflictTargetClause(d, conflict, update)
}

func (sqliteDialect) ClassifyError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	// SQLite reports "UNIQUE constraint failed: users.email, users.tenant_id".
	switch {
	case strings.Contains(message, "UNIQUE constraint failed"):
		table, columns := sqliteConstraintTargets(message, "UNIQUE constraint failed:")
		return &ConstraintError{Kind: ConstraintUnique, Table: table, Columns: columns, Cause: err}
	case strings.Contains(message, "FOREIGN KEY constraint failed"):
		return &ConstraintError{Kind: ConstraintForeignKey, Cause: err}
	case strings.Contains(message, "NOT NULL constraint failed"):
		table, columns := sqliteConstraintTargets(message, "NOT NULL constraint failed:")
		return &ConstraintError{Kind: ConstraintNotNull, Table: table, Columns: columns, Cause: err}
	case strings.Contains(message, "CHECK constraint failed"):
		table, columns := sqliteConstraintTargets(message, "CHECK constraint failed:")
		return &ConstraintError{Kind: ConstraintCheck, Table: table, Columns: columns, Cause: err}
	}
	return err
}

func sqliteConstraintTargets(message, prefix string) (string, []string) {
	_, rest, found := strings.Cut(message, prefix)
	if !found {
		return "", nil
	}
	var table string
	var columns []string
	for _, target := range strings.Split(rest, ",") {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if qualifier, column, ok := strings.Cut(target, "."); ok {
			table = qualifier
			columns = append(columns, column)
			continue
		}
		columns = append(columns, target)
	}
	return table, columns
}

// quotedSegment extracts the first single- or double-quoted run from a driver
// message, which is where Postgres and MySQL put the constraint name.
func quotedSegment(message string) string {
	for _, quote := range []byte{'"', '\''} {
		start := strings.IndexByte(message, quote)
		if start < 0 {
			continue
		}
		end := strings.IndexByte(message[start+1:], quote)
		if end < 0 {
			continue
		}
		return message[start+1 : start+1+end]
	}
	return ""
}

func columnsFromQuoted(message string) []string {
	if name := quotedSegment(message); name != "" {
		return []string{name}
	}
	return nil
}

func Postgres() Dialect { return postgresDialect{} }
func MySQL() Dialect    { return mysqlDialect{} }
func SQLite() Dialect   { return sqliteDialect{} }
