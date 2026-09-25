package orm

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gertd/go-pluralize"
	"github.com/iancoleman/strcase"
)

// RelationKind enumerates the relationship shapes the mapper understands.
type RelationKind int

const (
	RelationNone RelationKind = iota
	HasOne
	HasMany
	BelongsTo
	ManyToMany
)

func (k RelationKind) String() string {
	switch k {
	case HasOne:
		return "hasOne"
	case HasMany:
		return "hasMany"
	case BelongsTo:
		return "belongsTo"
	case ManyToMany:
		return "many2many"
	}
	return "none"
}

// RelationInfo describes one association declared on a model.
//
// Target metadata is resolved lazily, never during collectFields: two models
// that reference each other (User.Posts and Post.User) would otherwise recurse
// forever through modelInfoFor.
type RelationInfo struct {
	Name   string
	Kind   RelationKind
	Target reflect.Type
	Index  []int

	// ForeignKeyField names the struct field holding the foreign key. For
	// HasOne and HasMany it lives on Target; for BelongsTo it lives on the
	// declaring model.
	ForeignKeyField string

	// ReferencesField names the key field on the opposite side. Empty means
	// the primary key.
	ReferencesField string

	// Pivot describes the join table for ManyToMany. Populated at parse time
	// so the shape stays stable once many-to-many lands, but not yet used.
	PivotTable      string
	PivotForeignKey string
	PivotTargetKey  string
}

type ModelInfo struct {
	Type     reflect.Type
	Table    string
	Fields   []FieldInfo
	ByColumn map[string]FieldInfo
	ByName   map[string]FieldInfo
	// PrimaryKeys holds every primary-key column in declaration order.
	// PrimaryKey is the single key, or nil when the model has none or a
	// composite one.
	PrimaryKeys []FieldInfo
	PrimaryKey  *FieldInfo

	Relations  []RelationInfo
	ByRelation map[string]RelationInfo

	// Lifecycle columns, detected by tag or by conventional name. Nil when
	// the model does not declare one.
	SoftDelete *FieldInfo
	CreatedAt  *FieldInfo
	UpdatedAt  *FieldInfo
}

// HasSoftDelete reports whether reads should hide soft-deleted rows.
func (m *ModelInfo) HasSoftDelete() bool { return m.SoftDelete != nil }

// HasCompositeKey reports whether the model is keyed on more than one column.
func (m *ModelInfo) HasCompositeKey() bool { return len(m.PrimaryKeys) > 1 }

// singleKey returns the sole primary key, or an error explaining why there
// isn't one. Operations that can only address one column route through here so
// the message is consistent.
func (m *ModelInfo) singleKey(operation string) (FieldInfo, error) {
	switch len(m.PrimaryKeys) {
	case 1:
		return m.PrimaryKeys[0], nil
	case 0:
		return FieldInfo{}, fmt.Errorf("orm: model %s has no primary key", m.Type)
	}
	columns := make([]string, len(m.PrimaryKeys))
	for i, key := range m.PrimaryKeys {
		columns[i] = key.Column
	}
	return FieldInfo{}, fmt.Errorf("orm: %s needs a single primary key, but %s is keyed on (%s)",
		operation, m.Type, strings.Join(columns, ", "))
}

// Column returns the column name for a struct field name.
func (m *ModelInfo) Column(fieldName string) (string, bool) {
	field, ok := m.ByName[fieldName]
	if !ok {
		return "", false
	}
	return field.Column, true
}

type FieldInfo struct {
	Name          string
	Column        string
	Index         []int
	Type          reflect.Type
	PrimaryKey    bool
	AutoIncrement bool
}

// TableNamer lets a model override the inflected table name.
type TableNamer interface{ TableName() string }

var (
	modelInfoCache sync.Map
	inflector      = pluralize.NewClient()

	scannerType  = reflect.TypeFor[sql.Scanner]()
	valuerType   = reflect.TypeFor[driver.Valuer]()
	timeType     = reflect.TypeFor[time.Time]()
	nullTimeType = reflect.TypeFor[sql.NullTime]()
	bytesType    = reflect.TypeFor[[]byte]()
)

func ModelInfoFor[T any]() (*ModelInfo, error) {
	return modelInfoFor(reflect.TypeFor[T]())
}

func modelInfoFor(typ reflect.Type) (*ModelInfo, error) {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if cached, ok := modelInfoCache.Load(typ); ok {
		return cached.(*ModelInfo), nil
	}
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("orm: model %s must be a struct", typ)
	}

	info := &ModelInfo{
		Type:       typ,
		Table:      resolveTableName(typ),
		ByColumn:   make(map[string]FieldInfo),
		ByName:     make(map[string]FieldInfo),
		ByRelation: make(map[string]RelationInfo),
	}
	if err := collectFields(typ, nil, info); err != nil {
		return nil, err
	}
	if len(info.Fields) == 0 {
		return nil, fmt.Errorf("orm: model %s has no mappable fields", typ)
	}
	for i := range info.Fields {
		if info.Fields[i].PrimaryKey {
			info.PrimaryKeys = append(info.PrimaryKeys, info.Fields[i])
		}
	}
	if len(info.PrimaryKeys) == 1 {
		info.PrimaryKey = &info.PrimaryKeys[0]
	}
	if err := detectLifecycleColumns(typ, info); err != nil {
		return nil, err
	}

	actual, _ := modelInfoCache.LoadOrStore(typ, info)
	return actual.(*ModelInfo), nil
}

// detectLifecycleColumns wires up soft deletes and automatic timestamps,
// either from an explicit tag or from the conventional field name.
//
// The conventional match additionally requires a time-like type, so a model
// carrying its own `CreatedAt int64` keeps full manual control instead of
// having the ORM quietly overwrite it.
func detectLifecycleColumns(typ reflect.Type, info *ModelInfo) error {
	tagged := taggedLifecycleFields(typ)

	for i := range info.Fields {
		field := &info.Fields[i]
		switch {
		case tagged[field.Name] == "softDelete":
			// Asked for explicitly, so refuse rather than silently misbehave:
			// a non-nullable column can never satisfy "deleted_at IS NULL",
			// which would hide every row in the table.
			if !isNullableTime(field.Type) {
				return fmt.Errorf(
					"orm: soft-delete column %s.%s must be *time.Time or sql.NullTime, got %s",
					typ.Name(), field.Name, field.Type)
			}
			info.SoftDelete = field

		case field.Name == "DeletedAt" && isNullableTime(field.Type):
			info.SoftDelete = field

		case tagged[field.Name] == "autoCreateTime",
			field.Name == "CreatedAt" && isTimeLike(field.Type):
			info.CreatedAt = field

		case tagged[field.Name] == "autoUpdateTime",
			field.Name == "UpdatedAt" && isTimeLike(field.Type):
			info.UpdatedAt = field
		}
	}
	return nil
}

// isNullableTime reports whether a field can represent SQL NULL, which soft
// deletes depend on. A bare time.Time cannot: its zero value still writes a
// real timestamp.
func isNullableTime(typ reflect.Type) bool {
	if typ == nil {
		return false
	}
	if typ.Kind() == reflect.Pointer {
		return typ.Elem() == timeType
	}
	return typ == nullTimeType
}

func taggedLifecycleFields(typ reflect.Type) map[string]string {
	tagged := make(map[string]string)
	var walk func(reflect.Type)
	walk = func(current reflect.Type) {
		for i := 0; i < current.NumField(); i++ {
			field := current.Field(i)
			if field.Anonymous && field.Type.Kind() == reflect.Struct {
				walk(field.Type)
			}
			tag := parseORMTag(structTag(field))
			switch {
			case tag.softDelete:
				tagged[field.Name] = "softDelete"
			case tag.autoCreateTime:
				tagged[field.Name] = "autoCreateTime"
			case tag.autoUpdateTime:
				tagged[field.Name] = "autoUpdateTime"
			}
		}
	}
	walk(typ)
	return tagged
}

// isTimeLike reports whether a field can hold a timestamp.
func isTimeLike(typ reflect.Type) bool {
	if typ == nil {
		return false
	}
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ == timeType || typ == nullTimeType
}

// setTime writes a timestamp into a time.Time, *time.Time or sql.NullTime.
func setTime(field reflect.Value, when time.Time) error {
	if field.Kind() == reflect.Pointer {
		pointer := reflect.New(field.Type().Elem())
		if err := setTime(pointer.Elem(), when); err != nil {
			return err
		}
		field.Set(pointer)
		return nil
	}
	switch field.Type() {
	case timeType:
		field.Set(reflect.ValueOf(when))
		return nil
	case nullTimeType:
		field.Set(reflect.ValueOf(sql.NullTime{Time: when, Valid: true}))
		return nil
	}
	return fmt.Errorf("orm: cannot store a timestamp in %s", field.Type())
}

// timeIsSet reports whether a timestamp field already holds a value.
func timeIsSet(field reflect.Value) bool {
	if field.Kind() == reflect.Pointer {
		return !field.IsNil()
	}
	if field.Type() == nullTimeType {
		return field.Interface().(sql.NullTime).Valid
	}
	return !field.IsZero()
}

// resolveTableName honours TableNamer, then falls back to the same
// snake_case + pluralize pipeline the CLI and migration modules use, so an
// ORM query and a generated migration agree on the table name.
func resolveTableName(typ reflect.Type) string {
	if namer, ok := reflect.New(typ).Interface().(TableNamer); ok {
		if name := namer.TableName(); name != "" {
			return name
		}
	}
	return tableNameFromType(typ)
}

// pivotKeyFor derives the conventional pivot column for a model: User -> user_id.
func pivotKeyFor(typ reflect.Type) string {
	return inflector.Singular(snakeCase(typ.Name())) + "_id"
}

// snakeCase is the single column-naming rule, shared by the runtime and the
// exported naming helpers in naming.go.
func snakeCase(value string) string { return strcase.ToSnake(value) }

func tableNameFromType(typ reflect.Type) string {
	name := typ.Name()
	if name == "" {
		return ""
	}
	return inflector.Plural(snakeCase(name))
}

func collectFields(typ reflect.Type, parent []int, info *ModelInfo) error {
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}
		tag := parseORMTag(structTag(field))
		if tag.skip {
			continue
		}
		index := append(append([]int(nil), parent...), i)

		if field.Anonymous && field.Type.Kind() == reflect.Struct && tag.column == "" && tag.relation == RelationNone {
			if err := collectFields(field.Type, index, info); err != nil {
				return err
			}
			continue
		}
		if field.PkgPath != "" {
			continue
		}

		if tag.relation != RelationNone {
			relation, err := newRelationInfo(field, index, tag)
			if err != nil {
				return err
			}
			info.Relations = append(info.Relations, relation)
			info.ByRelation[relation.Name] = relation
			continue
		}

		// Fields that cannot round-trip through a SQL column (slices of
		// structs, nested models) are skipped rather than mapped. Before this
		// check an untagged `Posts []Post` became a bogus "posts" column.
		if !isColumnType(field.Type) {
			continue
		}

		column := tag.column
		if column == "" {
			column = snakeCase(field.Name)
		}
		if _, exists := info.ByColumn[column]; exists {
			return fmt.Errorf("orm: duplicate column %q on model %s", column, typ)
		}
		metadata := FieldInfo{
			Name:          field.Name,
			Column:        column,
			Index:         index,
			Type:          field.Type,
			PrimaryKey:    tag.primaryKey,
			AutoIncrement: tag.autoIncrement,
		}
		info.Fields = append(info.Fields, metadata)
		info.ByColumn[column] = metadata
		info.ByName[field.Name] = metadata
	}
	return nil
}

func newRelationInfo(field reflect.StructField, index []int, tag ormTag) (RelationInfo, error) {
	target := field.Type
	for target.Kind() == reflect.Pointer || target.Kind() == reflect.Slice {
		target = target.Elem()
	}
	if target.Kind() != reflect.Struct {
		return RelationInfo{}, fmt.Errorf("orm: relation %q must target a struct, got %s", field.Name, field.Type)
	}
	if tag.relation == HasMany && field.Type.Kind() != reflect.Slice {
		return RelationInfo{}, fmt.Errorf("orm: hasMany relation %q must be a slice", field.Name)
	}
	if tag.relation != ManyToMany && tag.relationKey == "" {
		return RelationInfo{}, fmt.Errorf("orm: relation %q needs a foreign key, e.g. orm:\"%s:UserID\"", field.Name, tag.relation)
	}
	return RelationInfo{
		Name:            field.Name,
		Kind:            tag.relation,
		Target:          target,
		Index:           index,
		ForeignKeyField: tag.relationKey,
		ReferencesField: tag.references,
		PivotTable:      tag.pivotTable,
		PivotForeignKey: tag.pivotForeignKey,
		PivotTargetKey:  tag.pivotTargetKey,
	}, nil
}

// isColumnType reports whether a Go type can be stored in a single column.
func isColumnType(typ reflect.Type) bool {
	if typ.Implements(scannerType) || reflect.PointerTo(typ).Implements(scannerType) {
		return true
	}
	if typ.Implements(valuerType) || reflect.PointerTo(typ).Implements(valuerType) {
		return true
	}
	if typ == timeType || typ == bytesType {
		return true
	}
	switch typ.Kind() {
	case reflect.Pointer:
		return isColumnType(typ.Elem())
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	case reflect.Slice:
		return typ.Elem().Kind() == reflect.Uint8
	}
	return false
}

// structTag prefers the orm tag and falls back to db, so models generated by
// the CLI (which emit only `db:"..."`) map without edits.
func structTag(field reflect.StructField) string {
	if tag, ok := field.Tag.Lookup("orm"); ok {
		return tag
	}
	if tag, ok := field.Tag.Lookup("db"); ok {
		if tag == "-" {
			return "-"
		}
		// db tags carry modifiers such as `id,omitempty`.
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			return ""
		}
		return "column:" + name
	}
	return ""
}

type ormTag struct {
	column         string
	primaryKey     bool
	autoIncrement  bool
	skip           bool
	softDelete     bool
	autoCreateTime bool
	autoUpdateTime bool

	relation    RelationKind
	relationKey string
	references  string

	pivotTable      string
	pivotForeignKey string
	pivotTargetKey  string
}

var relationPrefixes = map[string]RelationKind{
	"hasOne:":    HasOne,
	"hasMany:":   HasMany,
	"belongsTo:": BelongsTo,
	"many2many:": ManyToMany,
}

func parseORMTag(value string) ormTag {
	var result ormTag
	for _, option := range strings.Split(value, ";") {
		option = strings.TrimSpace(option)
		switch {
		case option == "-":
			result.skip = true
			continue
		case option == "primaryKey":
			result.primaryKey = true
			continue
		case option == "autoIncrement":
			result.autoIncrement = true
			continue
		case option == "softDelete":
			result.softDelete = true
			continue
		case option == "autoCreateTime":
			result.autoCreateTime = true
			continue
		case option == "autoUpdateTime":
			result.autoUpdateTime = true
			continue
		case strings.HasPrefix(option, "column:"):
			result.column = strings.TrimPrefix(option, "column:")
			continue
		case strings.HasPrefix(option, "references:"):
			result.references = strings.TrimPrefix(option, "references:")
			continue
		case strings.HasPrefix(option, "joinForeignKey:"):
			result.pivotForeignKey = strings.TrimPrefix(option, "joinForeignKey:")
			continue
		case strings.HasPrefix(option, "joinTargetKey:"):
			result.pivotTargetKey = strings.TrimPrefix(option, "joinTargetKey:")
			continue
		}
		for prefix, kind := range relationPrefixes {
			if strings.HasPrefix(option, prefix) {
				result.relation = kind
				if kind == ManyToMany {
					result.pivotTable = strings.TrimPrefix(option, prefix)
				} else {
					result.relationKey = strings.TrimPrefix(option, prefix)
				}
				break
			}
		}
	}
	return result
}
