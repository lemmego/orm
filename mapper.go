package orm

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// discard swallows a column the destination does not map.
type discard struct{}

func (discard) Scan(any) error { return nil }

// scanRows materialises rows into []T.
//
// When strict is false, columns with no matching field are discarded instead
// of failing. Raw queries need that: they legitimately return columns the
// destination struct does not declare.
func scanRows[T any](rows *sql.Rows, strict bool) ([]T, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	typ := reflect.TypeFor[T]()
	if typ.Kind() != reflect.Struct || isColumnType(typ) {
		return scanScalarRows[T](rows, columns)
	}

	info, err := modelInfoFor(typ)
	if err != nil {
		return nil, err
	}

	var result []T
	for rows.Next() {
		value := reflect.New(typ).Elem()
		destinations, finishers, err := buildDestinations(value, info, columns, strict)
		if err != nil {
			return nil, err
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, err
		}
		for _, finish := range finishers {
			finish()
		}
		result = append(result, value.Interface().(T))
	}
	return result, rows.Err()
}

// scanScalarRows handles aggregate, Pluck and single-column raw results.
func scanScalarRows[T any](rows *sql.Rows, columns []string) ([]T, error) {
	if len(columns) != 1 {
		return nil, fmt.Errorf("orm: cannot scan %d columns into %s", len(columns), reflect.TypeFor[T]())
	}
	var result []T
	for rows.Next() {
		value := reflect.New(reflect.TypeFor[T]()).Elem()
		destination, finish := scanDestination(value)
		if err := rows.Scan(destination); err != nil {
			return nil, err
		}
		if finish != nil {
			finish()
		}
		result = append(result, value.Interface().(T))
	}
	return result, rows.Err()
}

func buildDestinations(value reflect.Value, info *ModelInfo, columns []string, strict bool) ([]any, []func(), error) {
	destinations := make([]any, len(columns))
	var finishers []func()

	for i, column := range columns {
		field, ok := info.ByColumn[column]
		if !ok {
			if strict {
				return nil, nil, newMappingError(info.Type.String(), column, nil)
			}
			destinations[i] = discard{}
			continue
		}
		fieldValue, err := fieldByIndex(value, field.Index)
		if err != nil {
			return nil, nil, newMappingError(info.Type.String(), column, err)
		}
		destination, finish := scanDestination(fieldValue)
		destinations[i] = destination
		if finish != nil {
			finishers = append(finishers, finish)
		}
	}
	return destinations, finishers, nil
}

// scanDestination returns a scan target for one field plus an optional
// finaliser.
//
// A plain non-pointer field cannot receive SQL NULL: database/sql has nowhere
// to put nil. Scanning through a **T indirection reuses the standard library's
// own conversion and leaves the field zero when the column is NULL, without
// forcing every nullable column to be declared as a pointer.
func scanDestination(field reflect.Value) (any, func()) {
	// Timestamps come back in whatever shape the driver chose. SQLite returns
	// a string unless the column's declared type is one the driver
	// recognises, so "DATETIME(6)" or "TEXT" yields text where "DATETIME"
	// yields time.Time. Route every time field through a scanner that accepts
	// both, rather than failing on a perfectly ordinary schema.
	if isTimeLike(field.Type()) {
		return timeScanner{target: field}, nil
	}

	addr := field.Addr()
	if field.Kind() == reflect.Pointer || addr.Type().Implements(scannerType) {
		return addr.Interface(), nil
	}
	holder := reflect.New(reflect.PointerTo(field.Type()))
	return holder.Interface(), func() {
		loaded := holder.Elem()
		if loaded.IsNil() {
			field.SetZero()
			return
		}
		field.Set(loaded.Elem())
	}
}

// scanRowsInto is the non-generic twin of scanRows. Relation loading only
// knows its target as a reflect.Type, so it cannot go through the generic
// path. It returns a slice value of type []info.Type.
func scanRowsInto(rows *sql.Rows, info *ModelInfo) (reflect.Value, error) {
	columns, err := rows.Columns()
	if err != nil {
		return reflect.Value{}, err
	}
	result := reflect.MakeSlice(reflect.SliceOf(info.Type), 0, 8)
	for rows.Next() {
		value := reflect.New(info.Type).Elem()
		destinations, finishers, err := buildDestinations(value, info, columns, false)
		if err != nil {
			return reflect.Value{}, err
		}
		if err := rows.Scan(destinations...); err != nil {
			return reflect.Value{}, err
		}
		for _, finish := range finishers {
			finish()
		}
		result = reflect.Append(result, value)
	}
	return result, rows.Err()
}

// fieldByIndex walks an index path, allocating intermediate pointers so
// embedded pointer structs are addressable.
func fieldByIndex(value reflect.Value, index []int) (reflect.Value, error) {
	for _, position := range index {
		if value.Kind() == reflect.Pointer {
			if value.IsNil() {
				if !value.CanSet() {
					return reflect.Value{}, fmt.Errorf("orm: cannot allocate embedded %s", value.Type())
				}
				value.Set(reflect.New(value.Type().Elem()))
			}
			value = value.Elem()
		}
		value = value.Field(position)
	}
	if !value.CanAddr() || !value.CanSet() {
		return reflect.Value{}, fmt.Errorf("orm: field is not settable")
	}
	return value, nil
}

// timeLayouts are tried in order against a textual timestamp. The first two
// cover SQLite's own output; the rest cover the spellings drivers and hand
// written schemas commonly produce.
var timeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02",
	"15:04:05",
}

// timeScanner reads a timestamp column into a time.Time, *time.Time or
// sql.NullTime field, whatever concrete shape the driver hands over.
type timeScanner struct{ target reflect.Value }

func (s timeScanner) Scan(src any) error {
	switch value := src.(type) {
	case nil:
		s.target.SetZero()
		return nil
	case time.Time:
		return setTime(s.target, value)
	case string:
		return s.parse(value)
	case []byte:
		return s.parse(string(value))
	case int64:
		// Unix seconds, which is how some schemas store timestamps.
		return setTime(s.target, time.Unix(value, 0).UTC())
	}
	return fmt.Errorf("orm: cannot read %T into %s", src, s.target.Type())
}

func (s timeScanner) parse(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		s.target.SetZero()
		return nil
	}
	for _, layout := range timeLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return setTime(s.target, parsed)
		}
	}
	return fmt.Errorf("orm: cannot parse %q as a timestamp for %s", value, s.target.Type())
}
