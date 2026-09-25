package orm

import "strings"

type predicateKind int

const (
	predicateEmpty predicateKind = iota
	predicateSimple
	predicateRaw
	predicateComposite
)

// Predicate is the single currency of the query DSL. String constructors
// (orm.Eq) and generated field descriptors (UserFields.Active.Eq) both build
// one, so there is exactly one Where signature and one SQL compiler.
//
// Predicates stay structured until compile time because quoting and
// placeholder numbering both depend on the dialect, which is not known when
// the predicate is constructed.
type Predicate struct {
	kind  predicateKind
	col   string
	op    string
	args  []any
	raw   string
	logic string
	parts []Predicate
}

// RawPredicate embeds literal SQL. Placeholders are written as ? and are
// renumbered per dialect; identifiers are passed through untouched.
func RawPredicate(sql string, args ...any) Predicate {
	return Predicate{kind: predicateRaw, raw: sql, args: append([]any(nil), args...)}
}

func simplePredicate(column, operator string, args ...any) Predicate {
	return Predicate{kind: predicateSimple, col: column, op: operator, args: args}
}

// IsZero reports whether the predicate would contribute no SQL.
func (p Predicate) IsZero() bool { return p.kind == predicateEmpty }

func (p Predicate) And(other Predicate) Predicate { return combine("AND", p, other) }
func (p Predicate) Or(other Predicate) Predicate  { return combine("OR", p, other) }

func combine(logic string, predicates ...Predicate) Predicate {
	parts := make([]Predicate, 0, len(predicates))
	for _, predicate := range predicates {
		if !predicate.IsZero() {
			parts = append(parts, predicate)
		}
	}
	switch len(parts) {
	case 0:
		return Predicate{}
	case 1:
		return parts[0]
	}
	return Predicate{kind: predicateComposite, logic: logic, parts: parts}
}

// And groups predicates with AND. Or groups them with OR.
func And(predicates ...Predicate) Predicate { return combine("AND", predicates...) }
func Or(predicates ...Predicate) Predicate  { return combine("OR", predicates...) }

// Not negates a predicate.
func Not(predicate Predicate) Predicate {
	if predicate.IsZero() {
		return predicate
	}
	return Predicate{kind: predicateComposite, logic: "NOT", parts: []Predicate{predicate}}
}

// String-based constructors. These take a column name and are the path the
// GPA adapter uses, since GPA delivers column names as runtime strings.
func Eq(column string, value any) Predicate  { return simplePredicate(column, "=", value) }
func Ne(column string, value any) Predicate  { return simplePredicate(column, "<>", value) }
func Gt(column string, value any) Predicate  { return simplePredicate(column, ">", value) }
func Gte(column string, value any) Predicate { return simplePredicate(column, ">=", value) }
func Lt(column string, value any) Predicate  { return simplePredicate(column, "<", value) }
func Lte(column string, value any) Predicate { return simplePredicate(column, "<=", value) }

func Like(column string, pattern string) Predicate {
	return simplePredicate(column, "LIKE", pattern)
}

func NotLike(column string, pattern string) Predicate {
	return simplePredicate(column, "NOT LIKE", pattern)
}

func In(column string, values ...any) Predicate {
	return simplePredicate(column, "IN", values...)
}

func NotIn(column string, values ...any) Predicate {
	return simplePredicate(column, "NOT IN", values...)
}

func IsNull(column string) Predicate    { return simplePredicate(column, "IS NULL") }
func IsNotNull(column string) Predicate { return simplePredicate(column, "IS NOT NULL") }

func Between(column string, low, high any) Predicate {
	return simplePredicate(column, "BETWEEN", low, high)
}

// Field is a model field descriptor. The Value type parameter prevents
// comparing, say, a bool column against a string. Descriptors are ordinary Go
// values: hand-write them now, generate them later.
type Field[Model, Value any] struct {
	column string
}

func NewField[Model, Value any](column string) Field[Model, Value] {
	return Field[Model, Value]{column: column}
}

// Column returns the underlying column name.
func (f Field[Model, Value]) Column() string { return f.column }

func (f Field[Model, Value]) Eq(value Value) Predicate  { return Eq(f.column, value) }
func (f Field[Model, Value]) Ne(value Value) Predicate  { return Ne(f.column, value) }
func (f Field[Model, Value]) Gt(value Value) Predicate  { return Gt(f.column, value) }
func (f Field[Model, Value]) Gte(value Value) Predicate { return Gte(f.column, value) }
func (f Field[Model, Value]) Lt(value Value) Predicate  { return Lt(f.column, value) }
func (f Field[Model, Value]) Lte(value Value) Predicate { return Lte(f.column, value) }

func (f Field[Model, Value]) In(values ...Value) Predicate {
	return In(f.column, toAnySlice(values)...)
}

func (f Field[Model, Value]) NotIn(values ...Value) Predicate {
	return NotIn(f.column, toAnySlice(values)...)
}

func (f Field[Model, Value]) Between(low, high Value) Predicate {
	return Between(f.column, low, high)
}

func (f Field[Model, Value]) IsNull() Predicate    { return IsNull(f.column) }
func (f Field[Model, Value]) IsNotNull() Predicate { return IsNotNull(f.column) }

func (f Field[Model, Value]) Asc() Ordering  { return Asc(f.column) }
func (f Field[Model, Value]) Desc() Ordering { return Desc(f.column) }

func toAnySlice[Value any](values []Value) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

// Ordering is one ORDER BY term.
type Ordering struct {
	column    string
	direction string
	raw       string
}

func Asc(column string) Ordering  { return Ordering{column: column, direction: "ASC"} }
func Desc(column string) Ordering { return Ordering{column: column, direction: "DESC"} }

// RawOrdering embeds literal ORDER BY SQL, for expressions the builder cannot
// express (COALESCE, CASE, NULLS LAST).
func RawOrdering(sql string) Ordering { return Ordering{raw: sql} }

func (o Ordering) compile(dialect Dialect) string {
	if o.raw != "" {
		return o.raw
	}
	if o.column == "" {
		return ""
	}
	return quoteColumn(dialect, o.column) + " " + o.direction
}

func (p Predicate) compile(dialect Dialect, next *int) (string, []any) {
	switch p.kind {
	case predicateEmpty:
		return "", nil

	case predicateRaw:
		return expandPlaceholders(dialect, p.raw, next), append([]any(nil), p.args...)

	case predicateSimple:
		return p.compileSimple(dialect, next)

	case predicateComposite:
		if p.logic == "NOT" {
			inner, args := p.parts[0].compile(dialect, next)
			return "NOT (" + inner + ")", args
		}
		compiled := make([]string, 0, len(p.parts))
		args := make([]any, 0, len(p.parts))
		for _, part := range p.parts {
			sql, partArgs := part.compile(dialect, next)
			if sql == "" {
				continue
			}
			compiled = append(compiled, "("+sql+")")
			args = append(args, partArgs...)
		}
		return strings.Join(compiled, " "+p.logic+" "), args
	}
	return "", nil
}

func (p Predicate) compileSimple(dialect Dialect, next *int) (string, []any) {
	column := quoteColumn(dialect, p.col)

	switch p.op {
	case "IS NULL", "IS NOT NULL":
		return column + " " + p.op, nil

	case "IN", "NOT IN":
		if len(p.args) == 0 {
			// An empty IN list matches nothing; an empty NOT IN matches all.
			if p.op == "IN" {
				return "1 = 0", nil
			}
			return "1 = 1", nil
		}
		placeholders := make([]string, len(p.args))
		for i := range p.args {
			placeholders[i] = nextPlaceholder(dialect, next)
		}
		return column + " " + p.op + " (" + strings.Join(placeholders, ", ") + ")", append([]any(nil), p.args...)

	case "BETWEEN":
		low := nextPlaceholder(dialect, next)
		high := nextPlaceholder(dialect, next)
		return column + " BETWEEN " + low + " AND " + high, append([]any(nil), p.args...)
	}

	return column + " " + p.op + " " + nextPlaceholder(dialect, next), append([]any(nil), p.args...)
}

func nextPlaceholder(dialect Dialect, next *int) string {
	*next++
	return dialect.Placeholder(*next)
}

// expandPlaceholders renumbers ? markers in user-supplied SQL so raw
// predicates compose with built ones under $N dialects.
func expandPlaceholders(dialect Dialect, sql string, next *int) string {
	if !strings.Contains(sql, "?") {
		return sql
	}
	var builder strings.Builder
	for _, char := range sql {
		if char == '?' {
			builder.WriteString(nextPlaceholder(dialect, next))
			continue
		}
		builder.WriteRune(char)
	}
	return builder.String()
}

// quoteColumn quotes a plain identifier or dotted identifier path and leaves
// anything else (function calls, expressions, *) alone.
func quoteColumn(dialect Dialect, column string) string {
	if column == "" || column == "*" || !isIdentifierPath(column) {
		return column
	}
	parts := strings.Split(column, ".")
	for i, part := range parts {
		parts[i] = dialect.Quote(part)
	}
	return strings.Join(parts, ".")
}

func isIdentifierPath(value string) bool {
	if value == "" {
		return false
	}
	for _, segment := range strings.Split(value, ".") {
		if segment == "" {
			return false
		}
		for i, char := range segment {
			switch {
			case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char == '_':
			case char >= '0' && char <= '9' && i > 0:
			default:
				return false
			}
		}
	}
	return true
}
