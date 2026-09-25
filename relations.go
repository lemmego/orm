package orm

import (
	"context"
	"fmt"
	"reflect"
	"strings"
)

// relationChunkSize bounds how many keys go into one IN list. SQLite caps
// bound parameters at 999 by default, so batches stay well under that.
const relationChunkSize = 400

// loadRelations eager-loads the paths registered with With.
func (q *Query[T]) loadRelations(ctx context.Context, values []T) error {
	info, err := modelInfoFor(reflect.TypeFor[T]())
	if err != nil {
		return err
	}
	// Slice elements are addressable through reflection, which is what lets
	// the loader assign into each parent in place.
	return loadPaths(ctx, q.executor, q.dialect, reflect.ValueOf(values), info, q.withs)
}

// groupPaths splits "Posts.Comments" into the first segment and the remainder,
// so each relation level is loaded once regardless of how many paths share it.
func groupPaths(paths []string) ([]string, map[string][]string) {
	var order []string
	nested := make(map[string][]string)
	for _, path := range paths {
		head, rest, _ := strings.Cut(path, ".")
		if head == "" {
			continue
		}
		if _, seen := nested[head]; !seen {
			order = append(order, head)
			nested[head] = nil
		}
		if rest != "" {
			nested[head] = append(nested[head], rest)
		}
	}
	return order, nested
}

// loadPaths issues exactly one query per relation level. Two hundred parents
// with one relation cost two queries in total, not two hundred and one.
func loadPaths(ctx context.Context, exec executor, dialect Dialect, parents reflect.Value, info *ModelInfo, paths []string) error {
	if parents.Len() == 0 {
		return nil
	}
	order, nested := groupPaths(paths)

	for _, name := range order {
		relation, ok := info.ByRelation[name]
		if !ok {
			return fmt.Errorf("orm: model %s has no relation %q", info.Type, name)
		}
		childInfo, err := modelInfoFor(relation.Target)
		if err != nil {
			return err
		}
		if relation.Kind == ManyToMany {
			if err := loadManyToMany(ctx, exec, dialect, parents, info, childInfo, relation, nested[name]); err != nil {
				return err
			}
			continue
		}
		parentField, childField, err := relationKeys(info, childInfo, relation)
		if err != nil {
			return err
		}
		keys, err := collectKeys(parents, parentField)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			continue
		}
		children, err := fetchByKeys(ctx, exec, dialect, childInfo, childField, keys)
		if err != nil {
			return err
		}

		// Deeper levels are loaded before the children are handed to their
		// parents. assignRelation copies struct values, so anything attached
		// to a child afterwards would never reach the parent's copy.
		if len(nested[name]) > 0 && children.Len() > 0 {
			if err := loadPaths(ctx, exec, dialect, children, childInfo, nested[name]); err != nil {
				return err
			}
		}
		if err := assignRelation(parents, children, info, childInfo, relation, parentField, childField); err != nil {
			return err
		}
	}
	return nil
}

// relationKeys resolves which field carries the key on each side.
func relationKeys(info, childInfo *ModelInfo, relation RelationInfo) (parentField, childField FieldInfo, err error) {
	switch relation.Kind {
	case HasOne, HasMany:
		if parentField, err = keyField(info, relation.ReferencesField); err != nil {
			return
		}
		childField, err = namedField(childInfo, relation.ForeignKeyField)

	case BelongsTo:
		if parentField, err = namedField(info, relation.ForeignKeyField); err != nil {
			return
		}
		childField, err = keyField(childInfo, relation.ReferencesField)

	case ManyToMany:
		err = &UnsupportedFeatureError{Dialect: "orm", Feature: "many-to-many relations"}

	default:
		err = fmt.Errorf("orm: relation %q has no kind", relation.Name)
	}
	return
}

func keyField(info *ModelInfo, fieldName string) (FieldInfo, error) {
	if fieldName != "" {
		return namedField(info, fieldName)
	}
	// Relations join on one column. Name it explicitly with references: when
	// the model has a composite key.
	return info.singleKey("relation loading")
}

func namedField(info *ModelInfo, fieldName string) (FieldInfo, error) {
	field, ok := info.ByName[fieldName]
	if !ok {
		return FieldInfo{}, fmt.Errorf("orm: model %s has no mapped field %q", info.Type, fieldName)
	}
	return field, nil
}

// collectKeys gathers the distinct, non-null key values across all parents.
func collectKeys(parents reflect.Value, field FieldInfo) ([]any, error) {
	seen := make(map[any]bool, parents.Len())
	keys := make([]any, 0, parents.Len())
	for i := 0; i < parents.Len(); i++ {
		value, err := fieldByIndex(parents.Index(i), field.Index)
		if err != nil {
			return nil, err
		}
		key, ok := normalizeKey(value)
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys, nil
}

// normalizeKey reduces a key to a comparable canonical form so an int32
// parent key and an int64 foreign key still match in the index map.
func normalizeKey(value reflect.Value) (any, bool) {
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, false
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(value.Uint()), true
	case reflect.Float32, reflect.Float64:
		return value.Float(), true
	case reflect.String:
		return value.String(), true
	}
	if !value.IsValid() || value.IsZero() {
		return nil, false
	}
	if !value.Type().Comparable() {
		return nil, false
	}
	return value.Interface(), true
}

func fetchByKeys(ctx context.Context, exec executor, dialect Dialect, info *ModelInfo, field FieldInfo, keys []any) (reflect.Value, error) {
	result := reflect.MakeSlice(reflect.SliceOf(info.Type), 0, len(keys))

	columns := make([]string, len(info.Fields))
	for i, column := range info.Fields {
		columns[i] = dialect.Quote(column.Column)
	}

	for start := 0; start < len(keys); start += relationChunkSize {
		end := min(start+relationChunkSize, len(keys))
		chunk := keys[start:end]

		next := 0
		placeholders := make([]string, len(chunk))
		for i := range chunk {
			placeholders[i] = nextPlaceholder(dialect, &next)
		}
		query := "SELECT " + strings.Join(columns, ", ") +
			" FROM " + dialect.Quote(info.Table) +
			" WHERE " + dialect.Quote(field.Column) +
			" IN (" + strings.Join(placeholders, ", ") + ")"

		// A soft-deleted row is hidden from direct queries, so it must be
		// hidden from relations too. Without this a deleted child still
		// appeared through its parent's eager load.
		if info.SoftDelete != nil {
			query += " AND " + dialect.Quote(info.SoftDelete.Column) + " IS NULL"
		}

		rows, err := exec.QueryContext(ctx, query, chunk...)
		if err != nil {
			return reflect.Value{}, dialect.ClassifyError(err)
		}
		batch, err := scanRowsInto(rows, info)
		rows.Close()
		if err != nil {
			return reflect.Value{}, err
		}
		result = reflect.AppendSlice(result, batch)
	}
	return result, nil
}

func assignRelation(parents, children reflect.Value, info, childInfo *ModelInfo, relation RelationInfo, parentField, childField FieldInfo) error {
	// Index the children by the key they join on.
	index := make(map[any][]reflect.Value, children.Len())
	for i := 0; i < children.Len(); i++ {
		value, err := fieldByIndex(children.Index(i), childField.Index)
		if err != nil {
			return err
		}
		key, ok := normalizeKey(value)
		if !ok {
			continue
		}
		index[key] = append(index[key], children.Index(i))
	}

	for i := 0; i < parents.Len(); i++ {
		parent := parents.Index(i)
		keyValue, err := fieldByIndex(parent, parentField.Index)
		if err != nil {
			return err
		}
		key, ok := normalizeKey(keyValue)
		if !ok {
			continue
		}
		target, err := fieldByIndex(parent, relation.Index)
		if err != nil {
			return err
		}
		matches := index[key]

		if relation.Kind == HasMany {
			slice := reflect.MakeSlice(target.Type(), 0, len(matches))
			for _, match := range matches {
				slice = reflect.Append(slice, match)
			}
			target.Set(slice)
			continue
		}
		if len(matches) == 0 {
			continue
		}
		if err := setRelated(target, matches[0]); err != nil {
			return err
		}
	}
	return nil
}

// setRelated assigns a single related row, allocating when the field is a
// pointer.
func setRelated(target, value reflect.Value) error {
	if target.Kind() == reflect.Pointer {
		pointer := reflect.New(target.Type().Elem())
		pointer.Elem().Set(value)
		target.Set(pointer)
		return nil
	}
	if !value.Type().AssignableTo(target.Type()) {
		return fmt.Errorf("orm: cannot assign %s to %s", value.Type(), target.Type())
	}
	target.Set(value)
	return nil
}

// RelationHandle operates on one association of one parent record.
//
// The receiver is the database handle rather than the model, so models stay
// plain structs: a struct that carries a live connection cannot be safely
// serialised, copied between goroutines, or built in a test without a database.
type RelationHandle[Parent, Child any] struct {
	db       *DB
	executor executor
	dialect  Dialect
	parent   *Parent
	relation RelationInfo
	info     *ModelInfo
	child    *ModelInfo
	err      error
}

// Relation opens a handle onto the named association of parent.
func (db *DB) Relation[Parent, Child any](parent *Parent, name string) *RelationHandle[Parent, Child] {
	return newRelationHandle[Parent, Child](db, db.executor(), parent, name)
}

// Relation mirrors DB.Relation inside a transaction.
func (tx *Tx) Relation[Parent, Child any](parent *Parent, name string) *RelationHandle[Parent, Child] {
	return newRelationHandle[Parent, Child](tx.db, tx.executor(), parent, name)
}

func newRelationHandle[Parent, Child any](db *DB, exec executor, parent *Parent, name string) *RelationHandle[Parent, Child] {
	handle := &RelationHandle[Parent, Child]{db: db, executor: exec, dialect: db.dialect, parent: parent}
	if parent == nil {
		handle.err = fmt.Errorf("orm: relation %q needs a parent", name)
		return handle
	}

	info, err := modelInfoFor(reflect.TypeFor[Parent]())
	if err != nil {
		handle.err = err
		return handle
	}
	relation, ok := info.ByRelation[name]
	if !ok {
		handle.err = fmt.Errorf("orm: model %s has no relation %q", info.Type, name)
		return handle
	}
	if relation.Target != reflect.TypeFor[Child]() {
		handle.err = fmt.Errorf("orm: relation %q targets %s, not %s", name, relation.Target, reflect.TypeFor[Child]())
		return handle
	}
	childInfo, err := modelInfoFor(relation.Target)
	if err != nil {
		handle.err = err
		return handle
	}
	handle.relation, handle.info, handle.child = relation, info, childInfo
	return handle
}

// Query returns a query over the related rows, ready for further filtering.
func (h *RelationHandle[Parent, Child]) Query() *Query[Child] {
	query := newQueryForTable[Child](h.db, h.executor, "")
	if h.err != nil {
		query.err = h.err
		return query
	}
	if h.relation.Kind == ManyToMany {
		return h.manyToManyQuery(query)
	}
	parentField, childField, err := relationKeys(h.info, h.child, h.relation)
	if err != nil {
		query.err = err
		return query
	}
	value, err := fieldByIndex(reflect.ValueOf(h.parent).Elem(), parentField.Index)
	if err != nil {
		query.err = err
		return query
	}
	query.table = h.child.Table
	return query.Where(Eq(childField.Column, value.Interface()))
}

// Load fetches the association and assigns it onto the parent.
func (h *RelationHandle[Parent, Child]) Load(ctx context.Context) error {
	if h.err != nil {
		return h.err
	}
	parents := reflect.MakeSlice(reflect.SliceOf(h.info.Type), 1, 1)
	parents.Index(0).Set(reflect.ValueOf(h.parent).Elem())

	if err := loadPaths(ctx, h.executor, h.dialect, parents, h.info, []string{h.relation.Name}); err != nil {
		return err
	}
	reflect.ValueOf(h.parent).Elem().Set(parents.Index(0))
	return nil
}

func (h *RelationHandle[Parent, Child]) Count(ctx context.Context) (int64, error) {
	if h.err != nil {
		return 0, h.err
	}
	return h.Query().Count(ctx)
}

// Add associates children with the parent, inserting them when they are new
// and updating the foreign key when they already exist.
func (h *RelationHandle[Parent, Child]) Add(ctx context.Context, children ...*Child) error {
	if h.err != nil {
		return h.err
	}
	if h.relation.Kind == ManyToMany {
		return h.Attach(ctx, children...)
	}
	query := newQueryForTable[Child](h.db, h.executor, h.child.Table)

	for _, child := range children {
		if child == nil {
			continue
		}
		if h.relation.Kind == BelongsTo {
			if err := h.pointParentAt(ctx, child); err != nil {
				return err
			}
			continue
		}
		if err := h.pointChildAtParent(child); err != nil {
			return err
		}
		if err := saveChild(ctx, query, h.child, child); err != nil {
			return err
		}
	}
	return nil
}

// Remove dissociates children by clearing the foreign key. It never deletes
// rows: dropping a record is a separate, explicit decision.
func (h *RelationHandle[Parent, Child]) Remove(ctx context.Context, children ...*Child) error {
	if h.err != nil {
		return h.err
	}
	if h.relation.Kind == ManyToMany {
		return h.Detach(ctx, children...)
	}

	if h.relation.Kind == BelongsTo {
		field, err := namedField(h.info, h.relation.ForeignKeyField)
		if err != nil {
			return err
		}
		target, err := fieldByIndex(reflect.ValueOf(h.parent).Elem(), field.Index)
		if err != nil {
			return err
		}
		target.SetZero()
		parentQuery := newQueryForTable[Parent](h.db, h.executor, h.info.Table)
		_, err = parentQuery.Update(ctx, h.parent)
		return err
	}

	childField, err := namedField(h.child, h.relation.ForeignKeyField)
	if err != nil {
		return err
	}
	query := newQueryForTable[Child](h.db, h.executor, h.child.Table)
	for _, child := range children {
		if child == nil {
			continue
		}
		target, err := fieldByIndex(reflect.ValueOf(child).Elem(), childField.Index)
		if err != nil {
			return err
		}
		target.SetZero()
		if _, err := query.Update(ctx, child); err != nil {
			return err
		}
	}
	return nil
}

// pointChildAtParent copies the parent key onto the child's foreign key.
func (h *RelationHandle[Parent, Child]) pointChildAtParent(child *Child) error {
	parentField, childField, err := relationKeys(h.info, h.child, h.relation)
	if err != nil {
		return err
	}
	source, err := fieldByIndex(reflect.ValueOf(h.parent).Elem(), parentField.Index)
	if err != nil {
		return err
	}
	target, err := fieldByIndex(reflect.ValueOf(child).Elem(), childField.Index)
	if err != nil {
		return err
	}
	return assignKey(target, source)
}

// pointParentAt copies the child key onto the parent's foreign key and saves
// the parent, which is what BelongsTo association means.
func (h *RelationHandle[Parent, Child]) pointParentAt(ctx context.Context, child *Child) error {
	parentField, childField, err := relationKeys(h.info, h.child, h.relation)
	if err != nil {
		return err
	}
	source, err := fieldByIndex(reflect.ValueOf(child).Elem(), childField.Index)
	if err != nil {
		return err
	}
	target, err := fieldByIndex(reflect.ValueOf(h.parent).Elem(), parentField.Index)
	if err != nil {
		return err
	}
	if err := assignKey(target, source); err != nil {
		return err
	}
	parentQuery := newQueryForTable[Parent](h.db, h.executor, h.info.Table)
	_, err = parentQuery.Update(ctx, h.parent)
	return err
}

// assignKey copies a key value across, converting between integer widths and
// allocating when the destination is a nullable pointer.
func assignKey(target, source reflect.Value) error {
	for source.Kind() == reflect.Pointer {
		if source.IsNil() {
			target.SetZero()
			return nil
		}
		source = source.Elem()
	}
	if target.Kind() == reflect.Pointer {
		pointer := reflect.New(target.Type().Elem())
		if err := assignKey(pointer.Elem(), source); err != nil {
			return err
		}
		target.Set(pointer)
		return nil
	}
	if !source.Type().ConvertibleTo(target.Type()) {
		return fmt.Errorf("orm: cannot use %s as foreign key of type %s", source.Type(), target.Type())
	}
	target.Set(source.Convert(target.Type()))
	return nil
}

// saveChild inserts a child that has no key yet, and updates one that does.
func saveChild[Child any](ctx context.Context, query *Query[Child], info *ModelInfo, child *Child) error {
	if info.PrimaryKey != nil {
		key, err := fieldByIndex(reflect.ValueOf(child).Elem(), info.PrimaryKey.Index)
		if err != nil {
			return err
		}
		if key.IsZero() {
			_, err := query.Create(ctx, child)
			return err
		}
	}
	_, err := query.Update(ctx, child)
	return err
}

// pivotPlan resolves everything a many-to-many operation needs: the join
// table, the two key columns in it, and the key fields on each side.
//
// Column names default to the conventional <singular model>_id and can be
// overridden with joinForeignKey: and joinTargetKey: in the tag.
type pivotPlan struct {
	table       string
	parentKey   string
	targetKey   string
	parentField FieldInfo
	childField  FieldInfo
}

func newPivotPlan(info, childInfo *ModelInfo, relation RelationInfo) (pivotPlan, error) {
	var plan pivotPlan
	if relation.PivotTable == "" {
		return plan, fmt.Errorf("orm: relation %q needs a pivot table, e.g. orm:\"many2many:user_roles\"", relation.Name)
	}
	parentField, err := keyField(info, relation.ReferencesField)
	if err != nil {
		return plan, err
	}
	childField, err := keyField(childInfo, "")
	if err != nil {
		return plan, err
	}

	plan = pivotPlan{
		table:       relation.PivotTable,
		parentKey:   relation.PivotForeignKey,
		targetKey:   relation.PivotTargetKey,
		parentField: parentField,
		childField:  childField,
	}
	if plan.parentKey == "" {
		plan.parentKey = pivotKeyFor(info.Type)
	}
	if plan.targetKey == "" {
		plan.targetKey = pivotKeyFor(childInfo.Type)
	}
	return plan, nil
}

// loadManyToMany costs two queries per relation level: one for the pivot rows
// and one for the target rows, regardless of how many parents there are.
func loadManyToMany(ctx context.Context, exec executor, dialect Dialect, parents reflect.Value, info, childInfo *ModelInfo, relation RelationInfo, nested []string) error {
	plan, err := newPivotPlan(info, childInfo, relation)
	if err != nil {
		return err
	}
	parentKeys, err := collectKeys(parents, plan.parentField)
	if err != nil {
		return err
	}
	if len(parentKeys) == 0 {
		return nil
	}

	links, targetKeys, err := fetchPivotLinks(ctx, exec, dialect, plan, parentKeys)
	if err != nil {
		return err
	}
	if len(targetKeys) == 0 {
		return assignManyToMany(parents, reflect.MakeSlice(reflect.SliceOf(relation.Target), 0, 0), relation, plan, links)
	}

	children, err := fetchByKeys(ctx, exec, dialect, childInfo, plan.childField, targetKeys)
	if err != nil {
		return err
	}
	// Nested levels resolve before attachment, since attaching copies values.
	if len(nested) > 0 && children.Len() > 0 {
		if err := loadPaths(ctx, exec, dialect, children, childInfo, nested); err != nil {
			return err
		}
	}
	return assignManyToMany(parents, children, relation, plan, links)
}

// fetchPivotLinks reads the join table and returns parent key -> target keys,
// plus the distinct target keys to fetch.
func fetchPivotLinks(ctx context.Context, exec executor, dialect Dialect, plan pivotPlan, parentKeys []any) (map[any][]any, []any, error) {
	links := make(map[any][]any, len(parentKeys))
	var targetKeys []any
	seen := make(map[any]bool)

	for start := 0; start < len(parentKeys); start += relationChunkSize {
		end := min(start+relationChunkSize, len(parentKeys))
		chunk := parentKeys[start:end]

		next := 0
		placeholders := make([]string, len(chunk))
		for i := range chunk {
			placeholders[i] = nextPlaceholder(dialect, &next)
		}
		query := "SELECT " + dialect.Quote(plan.parentKey) + ", " + dialect.Quote(plan.targetKey) +
			" FROM " + dialect.Quote(plan.table) +
			" WHERE " + dialect.Quote(plan.parentKey) +
			" IN (" + strings.Join(placeholders, ", ") + ")"

		rows, err := exec.QueryContext(ctx, query, chunk...)
		if err != nil {
			return nil, nil, dialect.ClassifyError(err)
		}
		for rows.Next() {
			var parent, target any
			if err := rows.Scan(&parent, &target); err != nil {
				rows.Close()
				return nil, nil, err
			}
			parentKey, parentOK := normalizeScanned(parent)
			targetKey, targetOK := normalizeScanned(target)
			if !parentOK || !targetOK {
				continue
			}
			links[parentKey] = append(links[parentKey], targetKey)
			if !seen[targetKey] {
				seen[targetKey] = true
				targetKeys = append(targetKeys, targetKey)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return links, targetKeys, nil
}

// normalizeScanned canonicalises a value scanned into any, so pivot keys
// compare equal to the keys normalizeKey produces from struct fields.
func normalizeScanned(value any) (any, bool) {
	switch typed := value.(type) {
	case nil:
		return nil, false
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case uint64:
		return int64(typed), true
	case float64:
		return typed, true
	case string:
		return typed, true
	case []byte:
		return string(typed), true
	}
	return normalizeKey(reflect.ValueOf(value))
}

func assignManyToMany(parents, children reflect.Value, relation RelationInfo, plan pivotPlan, links map[any][]any) error {
	index := make(map[any]reflect.Value, children.Len())
	for i := 0; i < children.Len(); i++ {
		value, err := fieldByIndex(children.Index(i), plan.childField.Index)
		if err != nil {
			return err
		}
		if key, ok := normalizeKey(value); ok {
			index[key] = children.Index(i)
		}
	}

	for i := 0; i < parents.Len(); i++ {
		parent := parents.Index(i)
		keyValue, err := fieldByIndex(parent, plan.parentField.Index)
		if err != nil {
			return err
		}
		key, ok := normalizeKey(keyValue)
		if !ok {
			continue
		}
		target, err := fieldByIndex(parent, relation.Index)
		if err != nil {
			return err
		}
		slice := reflect.MakeSlice(target.Type(), 0, len(links[key]))
		for _, targetKey := range links[key] {
			if child, found := index[targetKey]; found {
				slice = reflect.Append(slice, child)
			}
		}
		target.Set(slice)
	}
	return nil
}

// manyToManyQuery selects the target rows whose key appears in the pivot for
// this parent.
func (h *RelationHandle[Parent, Child]) manyToManyQuery(query *Query[Child]) *Query[Child] {
	plan, key, err := h.pivot()
	if err != nil {
		query.err = err
		return query
	}
	query.table = h.child.Table
	condition := h.dialect.Quote(plan.childField.Column) +
		" IN (SELECT " + h.dialect.Quote(plan.targetKey) +
		" FROM " + h.dialect.Quote(plan.table) +
		" WHERE " + h.dialect.Quote(plan.parentKey) + " = ?)"
	return query.Where(RawPredicate(condition, key))
}

// pivot resolves the join-table plan and this parent's key value.
func (h *RelationHandle[Parent, Child]) pivot() (pivotPlan, any, error) {
	if h.err != nil {
		return pivotPlan{}, nil, h.err
	}
	if h.relation.Kind != ManyToMany {
		return pivotPlan{}, nil, fmt.Errorf("orm: relation %q is %s, not many2many", h.relation.Name, h.relation.Kind)
	}
	plan, err := newPivotPlan(h.info, h.child, h.relation)
	if err != nil {
		return plan, nil, err
	}
	value, err := fieldByIndex(reflect.ValueOf(h.parent).Elem(), plan.parentField.Index)
	if err != nil {
		return plan, nil, err
	}
	key, ok := normalizeKey(value)
	if !ok {
		return plan, nil, fmt.Errorf("orm: parent %s has no key to associate on", h.info.Type)
	}
	return plan, key, nil
}

// Attach links children to the parent. It is idempotent: keys already present
// in the pivot are skipped rather than duplicated.
func (h *RelationHandle[Parent, Child]) Attach(ctx context.Context, children ...*Child) error {
	keys, err := h.childKeys(children)
	if err != nil {
		return err
	}
	return h.AttachKeys(ctx, keys...)
}

// Detach unlinks children. Calling it with none detaches everything.
func (h *RelationHandle[Parent, Child]) Detach(ctx context.Context, children ...*Child) error {
	keys, err := h.childKeys(children)
	if err != nil {
		return err
	}
	return h.DetachKeys(ctx, keys...)
}

// Sync makes the association exactly the given set, attaching what is missing
// and detaching what is no longer wanted.
func (h *RelationHandle[Parent, Child]) Sync(ctx context.Context, children ...*Child) error {
	keys, err := h.childKeys(children)
	if err != nil {
		return err
	}
	return h.SyncKeys(ctx, keys...)
}

// Toggle attaches keys that are absent and detaches those already present.
func (h *RelationHandle[Parent, Child]) Toggle(ctx context.Context, children ...*Child) error {
	keys, err := h.childKeys(children)
	if err != nil {
		return err
	}
	return h.ToggleKeys(ctx, keys...)
}

// AttachKeys, DetachKeys, SyncKeys and ToggleKeys are the key-based forms, for
// when the related rows are known by identifier rather than loaded.
func (h *RelationHandle[Parent, Child]) AttachKeys(ctx context.Context, keys ...any) error {
	plan, parentKey, err := h.pivot()
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	existing, err := h.pivotKeys(ctx, plan, parentKey)
	if err != nil {
		return err
	}

	var pending []any
	seen := make(map[any]bool)
	for _, key := range keys {
		normalized, ok := normalizeScanned(key)
		if !ok || existing[normalized] || seen[normalized] {
			continue
		}
		seen[normalized] = true
		pending = append(pending, normalized)
	}
	if len(pending) == 0 {
		return nil
	}

	next := 0
	tuples := make([]string, len(pending))
	args := make([]any, 0, len(pending)*2)
	for i, key := range pending {
		left := nextPlaceholder(h.dialect, &next)
		right := nextPlaceholder(h.dialect, &next)
		tuples[i] = "(" + left + ", " + right + ")"
		args = append(args, parentKey, key)
	}
	query := "INSERT INTO " + h.dialect.Quote(plan.table) +
		" (" + h.dialect.Quote(plan.parentKey) + ", " + h.dialect.Quote(plan.targetKey) +
		") VALUES " + strings.Join(tuples, ", ")

	if _, err := h.executor.ExecContext(ctx, query, args...); err != nil {
		return h.dialect.ClassifyError(err)
	}
	return nil
}

func (h *RelationHandle[Parent, Child]) DetachKeys(ctx context.Context, keys ...any) error {
	plan, parentKey, err := h.pivot()
	if err != nil {
		return err
	}
	next := 0
	query := "DELETE FROM " + h.dialect.Quote(plan.table) +
		" WHERE " + h.dialect.Quote(plan.parentKey) + " = " + nextPlaceholder(h.dialect, &next)
	args := []any{parentKey}

	if len(keys) > 0 {
		placeholders := make([]string, len(keys))
		for i, key := range keys {
			normalized, ok := normalizeScanned(key)
			if !ok {
				continue
			}
			placeholders[i] = nextPlaceholder(h.dialect, &next)
			args = append(args, normalized)
		}
		query += " AND " + h.dialect.Quote(plan.targetKey) + " IN (" + strings.Join(placeholders, ", ") + ")"
	}

	if _, err := h.executor.ExecContext(ctx, query, args...); err != nil {
		return h.dialect.ClassifyError(err)
	}
	return nil
}

func (h *RelationHandle[Parent, Child]) SyncKeys(ctx context.Context, keys ...any) error {
	plan, parentKey, err := h.pivot()
	if err != nil {
		return err
	}
	existing, err := h.pivotKeys(ctx, plan, parentKey)
	if err != nil {
		return err
	}

	wanted := make(map[any]bool, len(keys))
	for _, key := range keys {
		if normalized, ok := normalizeScanned(key); ok {
			wanted[normalized] = true
		}
	}

	var stale []any
	for key := range existing {
		if !wanted[key] {
			stale = append(stale, key)
		}
	}
	if len(stale) > 0 {
		if err := h.DetachKeys(ctx, stale...); err != nil {
			return err
		}
	}
	return h.AttachKeys(ctx, keys...)
}

func (h *RelationHandle[Parent, Child]) ToggleKeys(ctx context.Context, keys ...any) error {
	plan, parentKey, err := h.pivot()
	if err != nil {
		return err
	}
	existing, err := h.pivotKeys(ctx, plan, parentKey)
	if err != nil {
		return err
	}

	var attach, detach []any
	for _, key := range keys {
		normalized, ok := normalizeScanned(key)
		if !ok {
			continue
		}
		if existing[normalized] {
			detach = append(detach, normalized)
			continue
		}
		attach = append(attach, normalized)
	}
	if len(detach) > 0 {
		if err := h.DetachKeys(ctx, detach...); err != nil {
			return err
		}
	}
	if len(attach) > 0 {
		return h.AttachKeys(ctx, attach...)
	}
	return nil
}

// pivotKeys reads the target keys currently linked to this parent.
func (h *RelationHandle[Parent, Child]) pivotKeys(ctx context.Context, plan pivotPlan, parentKey any) (map[any]bool, error) {
	next := 0
	query := "SELECT " + h.dialect.Quote(plan.targetKey) +
		" FROM " + h.dialect.Quote(plan.table) +
		" WHERE " + h.dialect.Quote(plan.parentKey) + " = " + nextPlaceholder(h.dialect, &next)

	rows, err := h.executor.QueryContext(ctx, query, parentKey)
	if err != nil {
		return nil, h.dialect.ClassifyError(err)
	}
	defer rows.Close()

	keys := make(map[any]bool)
	for rows.Next() {
		var value any
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		if normalized, ok := normalizeScanned(value); ok {
			keys[normalized] = true
		}
	}
	return keys, rows.Err()
}

// childKeys extracts primary keys from loaded child entities.
func (h *RelationHandle[Parent, Child]) childKeys(children []*Child) ([]any, error) {
	if h.err != nil {
		return nil, h.err
	}
	childKey, err := h.child.singleKey("association writes")
	if err != nil {
		return nil, err
	}
	keys := make([]any, 0, len(children))
	for _, child := range children {
		if child == nil {
			continue
		}
		value, err := fieldByIndex(reflect.ValueOf(child).Elem(), childKey.Index)
		if err != nil {
			return nil, err
		}
		if key, ok := normalizeKey(value); ok {
			keys = append(keys, key)
		}
	}
	return keys, nil
}
