package orm

import (
	"database/sql"
	"fmt"
	"reflect"
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
