package orm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Numeric constrains the aggregate helpers that only make sense over numbers.
type Numeric interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

type joinClause struct {
	kind  string
	table string
	on    Predicate
}

// Query is an immutable, chainable query over T. Every builder method returns
// a copy, so a partially built query can be shared and branched safely.
type Query[T any] struct {
	db       *DB
	executor executor
	dialect  Dialect
	table    string

	columns  []string
	preds    []Predicate
	joins    []joinClause
	groups   []string
	having   []Predicate
	orders   []Ordering
	withs    []string
	limit    int
	offset   int
	distinct bool

	// softDeleteColumn is resolved once from T and then carried through
	// As[R], so a projection cannot accidentally read soft-deleted rows.
	softDeleteColumn string
	withTrashed      bool
	onlyTrashed      bool

	// err defers a construction failure (an unmappable model, say) to the
	// point of execution, so the builder chain stays free of error returns.
	err error
}

func newQuery[T any](db *DB, exec executor) *Query[T] {
	query := &Query[T]{db: db, executor: exec, dialect: db.dialect}
	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		query.err = err
		return query
	}
	query.table = info.Table
	if info.SoftDelete != nil {
		query.softDeleteColumn = info.SoftDelete.Column
	}
	return query
}

// newQueryForTable builds a query against an already-resolved table, for
// relation loading where the model is known but the entry point is not.
func newQueryForTable[T any](db *DB, exec executor, table string) *Query[T] {
	query := newQuery[T](db, exec)
	query.table = table
	return query
}

func (q *Query[T]) clone() *Query[T] {
	next := *q
	next.columns = append([]string(nil), q.columns...)
	next.preds = append([]Predicate(nil), q.preds...)
	next.joins = append([]joinClause(nil), q.joins...)
	next.groups = append([]string(nil), q.groups...)
	next.having = append([]Predicate(nil), q.having...)
	next.orders = append([]Ordering(nil), q.orders...)
	next.withs = append([]string(nil), q.withs...)
	return &next
}

// Table overrides the table this query targets.
func (q *Query[T]) Table(name string) *Query[T] {
	next := q.clone()
	next.table = name
	return next
}

// Where appends predicates, combined with AND.
func (q *Query[T]) Where(predicates ...Predicate) *Query[T] {
	next := q.clone()
	for _, predicate := range predicates {
		if !predicate.IsZero() {
			next.preds = append(next.preds, predicate)
		}
	}
	return next
}

// Select restricts the projection to the named columns.
func (q *Query[T]) Select(columns ...string) *Query[T] {
	next := q.clone()
	next.columns = append(next.columns, columns...)
	return next
}

func (q *Query[T]) Distinct() *Query[T] {
	next := q.clone()
	next.distinct = true
	return next
}

func (q *Query[T]) OrderBy(orderings ...Ordering) *Query[T] {
	next := q.clone()
	next.orders = append(next.orders, orderings...)
	return next
}

func (q *Query[T]) GroupBy(columns ...string) *Query[T] {
	next := q.clone()
	next.groups = append(next.groups, columns...)
	return next
}

func (q *Query[T]) Having(predicates ...Predicate) *Query[T] {
	next := q.clone()
	for _, predicate := range predicates {
		if !predicate.IsZero() {
			next.having = append(next.having, predicate)
		}
	}
	return next
}

func (q *Query[T]) Limit(limit int) *Query[T] {
	next := q.clone()
	next.limit = limit
	return next
}

func (q *Query[T]) Offset(offset int) *Query[T] {
	next := q.clone()
	next.offset = offset
	return next
}

// With eager-loads relations, including nested paths such as "Posts.Comments".
func (q *Query[T]) With(paths ...string) *Query[T] {
	next := q.clone()
	next.withs = append(next.withs, paths...)
	return next
}

// Join adds an INNER JOIN against the table of U. LeftJoin adds a LEFT JOIN.
// The type parameter supplies the joined table name, so the join stays in
// sync with the model rather than a hand-written string.
func (q *Query[T]) Join[U any](on Predicate) *Query[T] {
	return q.addJoin(reflect.TypeFor[U](), "INNER", on)
}

func (q *Query[T]) LeftJoin[U any](on Predicate) *Query[T] {
	return q.addJoin(reflect.TypeFor[U](), "LEFT", on)
}

// JoinTable and LeftJoinTable join a table by name, for the cases where the
// joined side has no model to infer it from — a lookup table, or a caller such
// as the GPA adapter that receives the table as a runtime string.
func (q *Query[T]) JoinTable(table string, on Predicate) *Query[T] {
	next := q.clone()
	next.joins = append(next.joins, joinClause{kind: "INNER", table: table, on: on})
	return next
}

func (q *Query[T]) LeftJoinTable(table string, on Predicate) *Query[T] {
	next := q.clone()
	next.joins = append(next.joins, joinClause{kind: "LEFT", table: table, on: on})
	return next
}

func (q *Query[T]) addJoin(target reflect.Type, kind string, on Predicate) *Query[T] {
	next := q.clone()
	info, err := modelInfoFor(target)
	if err != nil {
		next.err = err
		return next
	}
	next.joins = append(next.joins, joinClause{kind: kind, table: info.Table, on: on})
	return next
}

// As reprojects the query into a different result type while keeping the
// source table, predicates, joins and ordering. This is what makes joins and
// report DTOs usable without dropping to raw SQL: Query[T] would otherwise be
// permanently locked to returning []T.
func (q *Query[T]) As[R any]() *Query[R] {
	return &Query[R]{
		db:               q.db,
		executor:         q.executor,
		dialect:          q.dialect,
		table:            q.table,
		preds:            append([]Predicate(nil), q.preds...),
		joins:            append([]joinClause(nil), q.joins...),
		groups:           append([]string(nil), q.groups...),
		having:           append([]Predicate(nil), q.having...),
		orders:           append([]Ordering(nil), q.orders...),
		limit:            q.limit,
		offset:           q.offset,
		distinct:         q.distinct,
		softDeleteColumn: q.softDeleteColumn,
		withTrashed:      q.withTrashed,
		onlyTrashed:      q.onlyTrashed,
		err:              q.err,
	}
}

// WithTrashed includes soft-deleted rows. OnlyTrashed returns just those.
func (q *Query[T]) WithTrashed() *Query[T] {
	next := q.clone()
	next.withTrashed = true
	next.onlyTrashed = false
	return next
}

func (q *Query[T]) OnlyTrashed() *Query[T] {
	next := q.clone()
	next.onlyTrashed = true
	next.withTrashed = false
	return next
}

// Scope applies reusable query modifiers, so common filters can be named once
// and composed.
func (q *Query[T]) Scope(scopes ...Scope[T]) *Query[T] {
	next := q
	for _, scope := range scopes {
		if scope != nil {
			next = scope(next)
		}
	}
	return next
}

// Scope is a reusable query modifier.
type Scope[T any] func(*Query[T]) *Query[T]

// scopedPredicates returns the user predicates plus the implicit soft-delete
// filter. The two are kept apart because the bulk-mutation guard must only
// count predicates the caller actually wrote.
func (q *Query[T]) scopedPredicates() []Predicate {
	if q.softDeleteColumn == "" || q.withTrashed {
		return q.preds
	}
	filter := IsNull(q.softDeleteColumn)
	if q.onlyTrashed {
		filter = IsNotNull(q.softDeleteColumn)
	}
	return append(append([]Predicate(nil), q.preds...), filter)
}

// SQL renders the SELECT statement and its arguments.
func (q *Query[T]) SQL() (string, []any) {
	query, args, err := q.buildSelect("")
	if err != nil {
		return "", nil
	}
	return query, args
}

// buildSelect compiles the statement. Clauses are emitted in execution order
// so that a single placeholder counter numbers every argument correctly under
// $N dialects.
func (q *Query[T]) buildSelect(projection string) (string, []any, error) {
	if q.err != nil {
		return "", nil, q.err
	}
	if q.table == "" {
		return "", nil, fmt.Errorf("orm: no table for %s", reflect.TypeFor[T]())
	}

	var builder strings.Builder
	args := make([]any, 0, 8)
	next := 0

	builder.WriteString("SELECT ")
	if q.distinct {
		builder.WriteString("DISTINCT ")
	}
	if projection != "" {
		builder.WriteString(projection)
	} else {
		list, err := q.projection()
		if err != nil {
			return "", nil, err
		}
		builder.WriteString(list)
	}
	builder.WriteString(" FROM " + q.dialect.Quote(q.table))

	for _, join := range q.joins {
		builder.WriteString(" " + join.kind + " JOIN " + q.dialect.Quote(join.table))
		if !join.on.IsZero() {
			condition, conditionArgs := join.on.compile(q.dialect, &next)
			builder.WriteString(" ON " + condition)
			args = append(args, conditionArgs...)
		}
	}

	if clause, clauseArgs := compileConjunction(q.scopedPredicates(), q.dialect, &next); clause != "" {
		builder.WriteString(" WHERE " + clause)
		args = append(args, clauseArgs...)
	}

	if len(q.groups) > 0 {
		grouped := make([]string, len(q.groups))
		for i, column := range q.groups {
			grouped[i] = quoteColumn(q.dialect, column)
		}
		builder.WriteString(" GROUP BY " + strings.Join(grouped, ", "))
	}

	if clause, clauseArgs := compileConjunction(q.having, q.dialect, &next); clause != "" {
		builder.WriteString(" HAVING " + clause)
		args = append(args, clauseArgs...)
	}

	if len(q.orders) > 0 {
		ordered := make([]string, 0, len(q.orders))
		for _, ordering := range q.orders {
			if compiled := ordering.compile(q.dialect); compiled != "" {
				ordered = append(ordered, compiled)
			}
		}
		if len(ordered) > 0 {
			builder.WriteString(" ORDER BY " + strings.Join(ordered, ", "))
		}
	}

	if q.limit > 0 {
		builder.WriteString(" LIMIT " + nextPlaceholder(q.dialect, &next))
		args = append(args, q.limit)
	}
	if q.offset > 0 {
		builder.WriteString(" OFFSET " + nextPlaceholder(q.dialect, &next))
		args = append(args, q.offset)
	}
	return builder.String(), args, nil
}

// projection builds the explicit column list.
//
// The previous implementation emitted SELECT *, which fails against a mapper
// that rejects unknown columns: any column present in the table but absent
// from the struct broke every read. Naming the columns removes the failure
// mode entirely.
func (q *Query[T]) projection() (string, error) {
	if len(q.columns) > 0 {
		quoted := make([]string, len(q.columns))
		for i, column := range q.columns {
			quoted[i] = quoteColumn(q.dialect, column)
		}
		return strings.Join(quoted, ", "), nil
	}

	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		return "", err
	}
	qualify := len(q.joins) > 0
	quoted := make([]string, len(info.Fields))
	for i, field := range info.Fields {
		if qualify {
			quoted[i] = q.dialect.Quote(q.table) + "." + q.dialect.Quote(field.Column)
			continue
		}
		quoted[i] = q.dialect.Quote(field.Column)
	}
	return strings.Join(quoted, ", "), nil
}

func compileConjunction(predicates []Predicate, dialect Dialect, next *int) (string, []any) {
	compiled := make([]string, 0, len(predicates))
	args := make([]any, 0, len(predicates))
	for _, predicate := range predicates {
		sqlText, predicateArgs := predicate.compile(dialect, next)
		if sqlText == "" {
			continue
		}
		// A composite needs bracketing only when it sits beside another
		// predicate; on its own it already spans the whole clause.
		if predicate.kind == predicateComposite && len(predicates) > 1 {
			sqlText = "(" + sqlText + ")"
		}
		compiled = append(compiled, sqlText)
		args = append(args, predicateArgs...)
	}
	return strings.Join(compiled, " AND "), args
}

// All executes the query and eager-loads any requested relations.
func (q *Query[T]) All(ctx context.Context) ([]T, error) {
	query, args, err := q.buildSelect("")
	if err != nil {
		return nil, err
	}
	rows, err := q.executor.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, q.dialect.ClassifyError(err)
	}
	defer rows.Close()

	values, err := scanRows[T](rows, true)
	if err != nil {
		return nil, err
	}
	if len(q.withs) > 0 && len(values) > 0 {
		if err := q.loadRelations(ctx, values); err != nil {
			return nil, err
		}
	}
	return values, nil
}

// Page is one page of results plus the totals needed to render a pager.
type Page[T any] struct {
	Items      []T
	Page       int
	PerPage    int
	Total      int64
	TotalPages int
}

// HasNext and HasPrev report whether neighbouring pages exist.
func (p *Page[T]) HasNext() bool { return p.Page < p.TotalPages }
func (p *Page[T]) HasPrev() bool { return p.Page > 1 }

// Paginate returns one page of results together with the total row count.
// Pages are 1-based.
func (q *Query[T]) Paginate(ctx context.Context, page, perPage int) (*Page[T], error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 1
	}

	// The count deliberately drops any limit or offset already on the query,
	// so Total reflects the whole result set rather than the page.
	total, err := q.Limit(0).Offset(0).Count(ctx)
	if err != nil {
		return nil, err
	}

	items, err := q.Limit(perPage).Offset((page - 1) * perPage).All(ctx)
	if err != nil {
		return nil, err
	}

	totalPages := int((total + int64(perPage) - 1) / int64(perPage))
	return &Page[T]{Items: items, Page: page, PerPage: perPage, Total: total, TotalPages: totalPages}, nil
}

// First returns the first matching row, or ErrNotFound.
func (q *Query[T]) First(ctx context.Context) (*T, error) {
	values, err := q.Limit(1).All(ctx)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, ErrNotFound
	}
	return &values[0], nil
}

// Find looks a row up by primary key. A composite key takes one value per
// column, in declaration order.
func (q *Query[T]) Find(ctx context.Context, keys ...any) (*T, error) {
	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	if len(info.PrimaryKeys) == 0 {
		return nil, fmt.Errorf("orm: model %s has no primary key", info.Type)
	}
	if len(keys) != len(info.PrimaryKeys) {
		return nil, fmt.Errorf("orm: %s is keyed on %d columns, got %d key values",
			info.Type, len(info.PrimaryKeys), len(keys))
	}

	query := q
	for i, key := range info.PrimaryKeys {
		query = query.Where(Eq(key.Column, keys[i]))
	}
	return query.First(ctx)
}

// Pluck reads a single column across all matching rows.
func (q *Query[T]) Pluck[V any](ctx context.Context, column string) ([]V, error) {
	query, args, err := q.buildSelect(quoteColumn(q.dialect, column))
	if err != nil {
		return nil, err
	}
	rows, err := q.executor.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, q.dialect.ClassifyError(err)
	}
	defer rows.Close()

	return scanRows[V](rows, false)
}

// Sum, Avg, Min and Max evaluate an aggregate over the current predicates.
func (q *Query[T]) Sum[V Numeric](ctx context.Context, column string) (V, error) {
	return aggregate[T, V](ctx, q, "SUM", column)
}

func (q *Query[T]) Avg[V Numeric](ctx context.Context, column string) (V, error) {
	return aggregate[T, V](ctx, q, "AVG", column)
}

func (q *Query[T]) Min[V any](ctx context.Context, column string) (V, error) {
	return aggregate[T, V](ctx, q, "MIN", column)
}

func (q *Query[T]) Max[V any](ctx context.Context, column string) (V, error) {
	return aggregate[T, V](ctx, q, "MAX", column)
}

func aggregate[T, V any](ctx context.Context, q *Query[T], function, column string) (V, error) {
	var zero V
	projection := function + "(" + quoteColumn(q.dialect, column) + ")"
	query, args, err := q.unordered().buildSelect(projection)
	if err != nil {
		return zero, err
	}
	rows, err := q.executor.QueryContext(ctx, query, args...)
	if err != nil {
		return zero, q.dialect.ClassifyError(err)
	}
	defer rows.Close()

	values, err := scanRows[V](rows, false)
	if err != nil {
		return zero, err
	}
	if len(values) == 0 {
		return zero, nil
	}
	return values[0], nil
}

// unordered drops ORDER BY, which is meaningless for an aggregate and which
// Postgres rejects when the select list is an aggregate with no GROUP BY:
// "column must appear in the GROUP BY clause or be used in an aggregate
// function". Paginate hits this, since it counts the query it is about to
// page over.
func (q *Query[T]) unordered() *Query[T] {
	if len(q.orders) == 0 {
		return q
	}
	next := q.clone()
	next.orders = nil
	return next
}

// Count reports how many rows match. quoteColumn leaves "*" alone, so this
// renders as COUNT(*).
func (q *Query[T]) Count(ctx context.Context) (int64, error) {
	return aggregate[T, int64](ctx, q, "COUNT", "*")
}

func (q *Query[T]) Exists(ctx context.Context) (bool, error) {
	query, args, err := q.Limit(1).buildSelect("1")
	if err != nil {
		return false, err
	}
	rows, err := q.executor.QueryContext(ctx, query, args...)
	if err != nil {
		return false, q.dialect.ClassifyError(err)
	}
	defer rows.Close()

	if rows.Next() {
		return true, nil
	}
	return false, rows.Err()
}

// Create inserts one entity and writes any generated key back onto it.
func (q *Query[T]) Create(ctx context.Context, entity *T) (sql.Result, error) {
	if err := runBeforeCreate(ctx, entity); err != nil {
		return nil, err
	}
	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	// Timestamps are stamped after the hook so a hook that sets them
	// explicitly keeps the value it chose.
	if err := applyCreateTimestamps(info, entity); err != nil {
		return nil, err
	}
	query, args, err := q.InsertSQL(entity)
	if err != nil {
		return nil, err
	}

	result, err := q.insert(ctx, query, args, info, entity)
	if err != nil {
		return nil, err
	}
	if err := runAfterCreate(ctx, entity); err != nil {
		return nil, err
	}
	return result, nil
}

// insert runs the statement and populates a generated primary key, using
// RETURNING where the dialect has it and LastInsertId where it does not.
func (q *Query[T]) insert(ctx context.Context, query string, args []any, info *ModelInfo, entity *T) (sql.Result, error) {
	key := generatedKey(info, entity)

	if key.IsValid() && q.dialect.SupportsReturning() {
		query += " RETURNING " + q.dialect.Quote(info.PrimaryKey.Column)
		destination, finish := scanDestination(key)

		rows, err := q.executor.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, q.dialect.ClassifyError(err)
		}
		defer rows.Close()

		if !rows.Next() {
			if rowsErr := rows.Err(); rowsErr != nil {
				return nil, q.dialect.ClassifyError(rowsErr)
			}
			return nil, ErrNoRowsAffected
		}
		if err := rows.Scan(destination); err != nil {
			return nil, q.dialect.ClassifyError(err)
		}
		if finish != nil {
			finish()
		}
		return returningResult{}, nil
	}

	result, err := q.executor.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, q.dialect.ClassifyError(err)
	}
	if key.IsValid() {
		if id, idErr := result.LastInsertId(); idErr == nil && id != 0 {
			setIntegerKey(key, id)
		}
	}
	return result, nil
}

// generatedKey returns the settable primary-key field when it is an
// auto-increment column still holding its zero value.
func generatedKey[T any](info *ModelInfo, entity *T) reflect.Value {
	if info.PrimaryKey == nil || !info.PrimaryKey.AutoIncrement {
		return reflect.Value{}
	}
	field, err := fieldByIndex(reflect.ValueOf(entity).Elem(), info.PrimaryKey.Index)
	if err != nil || !field.IsZero() {
		return reflect.Value{}
	}
	return field
}

func setIntegerKey(field reflect.Value, id int64) {
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		field.SetInt(id)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if id >= 0 {
			field.SetUint(uint64(id))
		}
	}
}

// returningResult stands in for sql.Result on the RETURNING path, where the
// driver gives us a row rather than a result.
type returningResult struct{}

func (returningResult) LastInsertId() (int64, error) {
	return 0, errors.New("orm: LastInsertId is unavailable on a RETURNING insert")
}
func (returningResult) RowsAffected() (int64, error) { return 1, nil }

func (q *Query[T]) InsertSQL(entity *T) (string, []any, error) {
	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		return "", nil, err
	}
	value := reflect.ValueOf(entity)
	if value.IsNil() {
		return "", nil, fmt.Errorf("orm: cannot insert a nil %s", info.Type)
	}
	value = value.Elem()

	columns := make([]string, 0, len(info.Fields))
	placeholders := make([]string, 0, len(info.Fields))
	args := make([]any, 0, len(info.Fields))
	next := 0
	for _, field := range info.Fields {
		fieldValue, err := fieldByIndex(value, field.Index)
		if err != nil {
			return "", nil, err
		}
		if field.AutoIncrement && fieldValue.IsZero() {
			continue
		}
		columns = append(columns, q.dialect.Quote(field.Column))
		placeholders = append(placeholders, nextPlaceholder(q.dialect, &next))
		args = append(args, fieldValue.Interface())
	}
	if len(columns) == 0 {
		return "", nil, fmt.Errorf("orm: cannot insert %s without fields", info.Type)
	}
	query := "INSERT INTO " + q.dialect.Quote(info.Table) +
		" (" + strings.Join(columns, ", ") + ") VALUES (" +
		strings.Join(placeholders, ", ") + ")"
	return query, args, nil
}

// Upsert inserts the entity, or updates the named columns when a row with the
// same conflict columns already exists.
//
// With no update columns, every mapped column except the conflict columns and
// the auto-increment key is overwritten. Use InsertIgnore to insert only.
//
// A generated key is not written back: the statement may update an existing
// row rather than insert, so there is no new key to report.
func (q *Query[T]) Upsert(ctx context.Context, entity *T, conflict []string, update ...string) (sql.Result, error) {
	if len(conflict) == 0 {
		return nil, fmt.Errorf("orm: Upsert needs at least one conflict column")
	}
	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	if len(update) == 0 {
		update = updatableColumns(info, conflict)
		if len(update) == 0 {
			return nil, fmt.Errorf("orm: %s has no columns left to update on conflict", info.Type)
		}
	}
	return q.upsert(ctx, entity, info, conflict, update)
}

// InsertIgnore inserts the entity, doing nothing if it already exists.
func (q *Query[T]) InsertIgnore(ctx context.Context, entity *T, conflict ...string) (sql.Result, error) {
	if len(conflict) == 0 {
		return nil, fmt.Errorf("orm: InsertIgnore needs at least one conflict column")
	}
	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	return q.upsert(ctx, entity, info, conflict, nil)
}

func (q *Query[T]) upsert(ctx context.Context, entity *T, info *ModelInfo, conflict, update []string) (sql.Result, error) {
	if err := runBeforeCreate(ctx, entity); err != nil {
		return nil, err
	}
	if err := applyCreateTimestamps(info, entity); err != nil {
		return nil, err
	}

	query, args, err := q.InsertSQL(entity)
	if err != nil {
		return nil, err
	}
	clause, err := q.dialect.UpsertClause(conflict, update)
	if err != nil {
		return nil, err
	}

	result, err := q.executor.ExecContext(ctx, query+" "+clause, args...)
	if err != nil {
		return nil, q.dialect.ClassifyError(err)
	}
	if err := runAfterCreate(ctx, entity); err != nil {
		return nil, err
	}
	return result, nil
}

// updatableColumns lists the columns an upsert should overwrite by default:
// everything mapped, minus the conflict columns and the generated key.
func updatableColumns(info *ModelInfo, conflict []string) []string {
	excluded := make(map[string]bool, len(conflict))
	for _, column := range conflict {
		excluded[column] = true
	}

	columns := make([]string, 0, len(info.Fields))
	for _, field := range info.Fields {
		if excluded[field.Column] || field.AutoIncrement {
			continue
		}
		columns = append(columns, field.Column)
	}
	return columns
}

// CreateBatch inserts many entities in a single multi-row statement.
func (q *Query[T]) CreateBatch(ctx context.Context, entities []*T) (sql.Result, error) {
	if len(entities) == 0 {
		return nil, nil
	}
	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	for _, entity := range entities {
		if err := runBeforeCreate(ctx, entity); err != nil {
			return nil, err
		}
		if err := applyCreateTimestamps(info, entity); err != nil {
			return nil, err
		}
	}

	// Auto-increment keys are omitted so the database assigns them; every row
	// must therefore share one column list.
	columns := make([]string, 0, len(info.Fields))
	fields := make([]FieldInfo, 0, len(info.Fields))
	for _, field := range info.Fields {
		if field.AutoIncrement {
			continue
		}
		columns = append(columns, q.dialect.Quote(field.Column))
		fields = append(fields, field)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("orm: cannot batch insert %s without fields", info.Type)
	}

	args := make([]any, 0, len(entities)*len(fields))
	tuples := make([]string, 0, len(entities))
	next := 0
	for _, entity := range entities {
		value := reflect.ValueOf(entity)
		if value.IsNil() {
			return nil, fmt.Errorf("orm: cannot insert a nil %s", info.Type)
		}
		value = value.Elem()
		placeholders := make([]string, len(fields))
		for i, field := range fields {
			fieldValue, err := fieldByIndex(value, field.Index)
			if err != nil {
				return nil, err
			}
			placeholders[i] = nextPlaceholder(q.dialect, &next)
			args = append(args, fieldValue.Interface())
		}
		tuples = append(tuples, "("+strings.Join(placeholders, ", ")+")")
	}

	query := "INSERT INTO " + q.dialect.Quote(info.Table) +
		" (" + strings.Join(columns, ", ") + ") VALUES " + strings.Join(tuples, ", ")
	result, err := q.executor.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, q.dialect.ClassifyError(err)
	}
	for _, entity := range entities {
		if err := runAfterCreate(ctx, entity); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (q *Query[T]) Update(ctx context.Context, entity *T) (sql.Result, error) {
	if err := runBeforeUpdate(ctx, entity); err != nil {
		return nil, err
	}
	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	if err := applyUpdateTimestamp(info, entity); err != nil {
		return nil, err
	}
	query, args, err := q.UpdateSQL(entity)
	if err != nil {
		return nil, err
	}
	result, err := q.executor.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, q.dialect.ClassifyError(err)
	}
	if err := runAfterUpdate(ctx, entity); err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateSQL writes every non-key column, or only the named ones.
func (q *Query[T]) UpdateSQL(entity *T, columns ...string) (string, []any, error) {
	info, value, primaryKeys, err := q.entityKey(entity)
	if err != nil {
		return "", nil, err
	}
	only := make(map[string]bool, len(columns))
	for _, column := range columns {
		only[column] = true
	}
	isKey := make(map[string]bool, len(primaryKeys))
	for _, key := range primaryKeys {
		isKey[key.Column] = true
	}

	sets := make([]string, 0, len(info.Fields))
	args := make([]any, 0, len(info.Fields)+len(primaryKeys))
	next := 0
	for _, field := range info.Fields {
		if isKey[field.Column] {
			continue
		}
		if len(only) > 0 && !only[field.Column] {
			continue
		}
		fieldValue, fieldErr := fieldByIndex(value, field.Index)
		if fieldErr != nil {
			return "", nil, fieldErr
		}
		sets = append(sets, q.dialect.Quote(field.Column)+" = "+nextPlaceholder(q.dialect, &next))
		args = append(args, fieldValue.Interface())
	}
	if len(sets) == 0 {
		return "", nil, fmt.Errorf("orm: no columns to update on %s", info.Type)
	}

	where, err := q.keyPredicateSQL(value, primaryKeys, &args, &next)
	if err != nil {
		return "", nil, err
	}
	query := "UPDATE " + q.dialect.Quote(info.Table) + " SET " + strings.Join(sets, ", ") +
		" WHERE " + where
	return query, args, nil
}

// Delete removes the entity. When the model declares a soft-delete column the
// row is marked rather than removed; ForceDelete always removes it.
func (q *Query[T]) Delete(ctx context.Context, entity *T) (sql.Result, error) {
	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	if info.SoftDelete != nil {
		return q.softDelete(ctx, info, entity)
	}
	return q.forceDelete(ctx, entity)
}

// ForceDelete issues a real DELETE even for a soft-deletable model.
func (q *Query[T]) ForceDelete(ctx context.Context, entity *T) (sql.Result, error) {
	return q.forceDelete(ctx, entity)
}

func (q *Query[T]) forceDelete(ctx context.Context, entity *T) (sql.Result, error) {
	if err := runBeforeDelete(ctx, entity); err != nil {
		return nil, err
	}
	query, args, err := q.DeleteSQL(entity)
	if err != nil {
		return nil, err
	}
	result, err := q.executor.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, q.dialect.ClassifyError(err)
	}
	if err := runAfterDelete(ctx, entity); err != nil {
		return nil, err
	}
	return result, nil
}

func (q *Query[T]) softDelete(ctx context.Context, info *ModelInfo, entity *T) (sql.Result, error) {
	if err := runBeforeDelete(ctx, entity); err != nil {
		return nil, err
	}
	field, err := fieldByIndex(reflect.ValueOf(entity).Elem(), info.SoftDelete.Index)
	if err != nil {
		return nil, err
	}
	if err := setTime(field, time.Now().UTC()); err != nil {
		return nil, err
	}

	query, args, err := q.UpdateSQL(entity, info.SoftDelete.Column)
	if err != nil {
		return nil, err
	}
	result, err := q.executor.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, q.dialect.ClassifyError(err)
	}
	if err := runAfterDelete(ctx, entity); err != nil {
		return nil, err
	}
	return result, nil
}

// Restore clears the soft-delete marker on an entity.
func (q *Query[T]) Restore(ctx context.Context, entity *T) (sql.Result, error) {
	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	if info.SoftDelete == nil {
		return nil, fmt.Errorf("orm: model %s is not soft-deletable", info.Type)
	}
	field, err := fieldByIndex(reflect.ValueOf(entity).Elem(), info.SoftDelete.Index)
	if err != nil {
		return nil, err
	}
	field.SetZero()

	query, args, err := q.UpdateSQL(entity, info.SoftDelete.Column)
	if err != nil {
		return nil, err
	}
	result, err := q.executor.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, q.dialect.ClassifyError(err)
	}
	return result, nil
}

func (q *Query[T]) DeleteSQL(entity *T) (string, []any, error) {
	info, value, primaryKeys, err := q.entityKey(entity)
	if err != nil {
		return "", nil, err
	}
	args := make([]any, 0, len(primaryKeys))
	next := 0
	where, err := q.keyPredicateSQL(value, primaryKeys, &args, &next)
	if err != nil {
		return "", nil, err
	}
	return "DELETE FROM " + q.dialect.Quote(info.Table) + " WHERE " + where, args, nil
}

// UpdateAll applies a column/value map to every matching row.
//
// A predicate is mandatory: an unfiltered UPDATE is almost always a mistake,
// and ErrUnsafeMutation makes it an explicit opt-in rather than a silent
// table rewrite.
func (q *Query[T]) UpdateAll(ctx context.Context, values map[string]any) (sql.Result, error) {
	if q.err != nil {
		return nil, q.err
	}
	if len(q.preds) == 0 {
		return nil, ErrUnsafeMutation
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("orm: UpdateAll requires at least one column")
	}

	columns := make([]string, 0, len(values))
	for column := range values {
		columns = append(columns, column)
	}
	sort.Strings(columns)

	next := 0
	sets := make([]string, len(columns))
	args := make([]any, 0, len(columns))
	for i, column := range columns {
		sets[i] = quoteColumn(q.dialect, column) + " = " + nextPlaceholder(q.dialect, &next)
		args = append(args, values[column])
	}

	query := "UPDATE " + q.dialect.Quote(q.table) + " SET " + strings.Join(sets, ", ")
	clause, clauseArgs := compileConjunction(q.scopedPredicates(), q.dialect, &next)
	query += " WHERE " + clause
	args = append(args, clauseArgs...)

	result, err := q.executor.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, q.dialect.ClassifyError(err)
	}
	return result, nil
}

// DeleteAll removes every matching row. Like UpdateAll it refuses to run
// without a predicate.
func (q *Query[T]) DeleteAll(ctx context.Context) (sql.Result, error) {
	if q.err != nil {
		return nil, q.err
	}
	if len(q.preds) == 0 {
		return nil, ErrUnsafeMutation
	}
	if q.softDeleteColumn != "" && !q.withTrashed {
		return q.UpdateAll(ctx, map[string]any{q.softDeleteColumn: time.Now().UTC()})
	}
	return q.forceDeleteAll(ctx)
}

// ForceDeleteAll removes matching rows outright, even for a soft-deletable
// model. It carries the same predicate requirement as DeleteAll.
func (q *Query[T]) ForceDeleteAll(ctx context.Context) (sql.Result, error) {
	if q.err != nil {
		return nil, q.err
	}
	if len(q.preds) == 0 {
		return nil, ErrUnsafeMutation
	}
	return q.forceDeleteAll(ctx)
}

func (q *Query[T]) forceDeleteAll(ctx context.Context) (sql.Result, error) {
	next := 0
	clause, args := compileConjunction(q.scopedPredicates(), q.dialect, &next)
	query := "DELETE FROM " + q.dialect.Quote(q.table) + " WHERE " + clause

	result, err := q.executor.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, q.dialect.ClassifyError(err)
	}
	return result, nil
}

// applyCreateTimestamps fills CreatedAt and UpdatedAt when they are still
// unset, leaving any value the caller or a hook chose in place.
//
// Stamps are taken in UTC. A naive column (Postgres TIMESTAMP, MySQL DATETIME)
// keeps no offset, so writing local time and reading it back yields a value
// shifted by the machine's zone offset. Normalising to UTC makes the round
// trip exact whether or not the column is timezone-aware.
func applyCreateTimestamps[T any](info *ModelInfo, entity *T) error {
	if info.CreatedAt == nil && info.UpdatedAt == nil {
		return nil
	}
	now := time.Now().UTC()
	value := reflect.ValueOf(entity).Elem()
	for _, column := range []*FieldInfo{info.CreatedAt, info.UpdatedAt} {
		if column == nil {
			continue
		}
		field, err := fieldByIndex(value, column.Index)
		if err != nil {
			return err
		}
		if timeIsSet(field) {
			continue
		}
		if err := setTime(field, now); err != nil {
			return err
		}
	}
	return nil
}

// applyUpdateTimestamp always refreshes UpdatedAt, which is the point of it.
func applyUpdateTimestamp[T any](info *ModelInfo, entity *T) error {
	if info.UpdatedAt == nil {
		return nil
	}
	field, err := fieldByIndex(reflect.ValueOf(entity).Elem(), info.UpdatedAt.Index)
	if err != nil {
		return err
	}
	return setTime(field, time.Now().UTC())
}

func (q *Query[T]) entityKey(entity *T) (*ModelInfo, reflect.Value, []FieldInfo, error) {
	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		return nil, reflect.Value{}, nil, err
	}
	value := reflect.ValueOf(entity)
	if value.IsNil() {
		return nil, reflect.Value{}, nil, fmt.Errorf("orm: cannot mutate a nil %s", info.Type)
	}
	if len(info.PrimaryKeys) == 0 {
		return nil, reflect.Value{}, nil, fmt.Errorf("orm: model %s has no primary key", info.Type)
	}
	return info, value.Elem(), info.PrimaryKeys, nil
}

// keyPredicateSQL renders the WHERE clause addressing one row by its primary
// key, which is one column for most models and several for a composite key.
func (q *Query[T]) keyPredicateSQL(value reflect.Value, keys []FieldInfo, args *[]any, next *int) (string, error) {
	clauses := make([]string, len(keys))
	for i, key := range keys {
		keyValue, err := fieldByIndex(value, key.Index)
		if err != nil {
			return "", err
		}
		*args = append(*args, keyValue.Interface())
		clauses[i] = q.dialect.Quote(key.Column) + " = " + nextPlaceholder(q.dialect, next)
	}
	return strings.Join(clauses, " AND "), nil
}

// RawQuery is a typed handle over hand-written SQL.
type RawQuery[T any] struct {
	executor executor
	dialect  Dialect
	query    string
	args     []any
}

func (q *RawQuery[T]) SQL() (string, []any) { return q.query, q.args }

// All scans the result set. Columns the destination does not declare are
// discarded rather than treated as an error, since raw SQL routinely returns
// more than the struct models.
func (q *RawQuery[T]) All(ctx context.Context) ([]T, error) {
	rows, err := q.executor.QueryContext(ctx, q.query, q.args...)
	if err != nil {
		return nil, q.dialect.ClassifyError(err)
	}
	defer rows.Close()

	return scanRows[T](rows, false)
}

func (q *RawQuery[T]) First(ctx context.Context) (*T, error) {
	values, err := q.All(ctx)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, ErrNotFound
	}
	return &values[0], nil
}
