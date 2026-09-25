package orm

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrNotFound       = errors.New("orm: record not found")
	ErrClosed         = errors.New("orm: transaction is closed")
	ErrUnsafeMutation = errors.New("orm: update or delete requires a predicate")
	ErrUnsupported    = errors.New("orm: unsupported feature")
	ErrNoRowsAffected = errors.New("orm: no rows affected")
	ErrConstraint     = errors.New("orm: constraint violated")
)

// ConstraintKind classifies a constraint violation reported by the driver.
type ConstraintKind int

const (
	ConstraintUnknown ConstraintKind = iota
	ConstraintUnique
	ConstraintForeignKey
	ConstraintNotNull
	ConstraintCheck
)

func (k ConstraintKind) String() string {
	switch k {
	case ConstraintUnique:
		return "unique"
	case ConstraintForeignKey:
		return "foreign key"
	case ConstraintNotNull:
		return "not null"
	case ConstraintCheck:
		return "check"
	}
	return "unknown"
}

// ConstraintError is the normalised form of a driver constraint violation.
// Dialect.ClassifyError builds it; the fields it can fill vary by driver.
type ConstraintError struct {
	Kind       ConstraintKind
	Constraint string
	Table      string
	Columns    []string
	Cause      error
}

func (e *ConstraintError) Error() string {
	var builder strings.Builder
	builder.WriteString("orm: ")
	builder.WriteString(e.Kind.String())
	builder.WriteString(" constraint violated")
	if e.Constraint != "" {
		fmt.Fprintf(&builder, " (%s)", e.Constraint)
	}
	if e.Table != "" {
		builder.WriteString(" on " + e.Table)
	}
	if len(e.Columns) > 0 {
		builder.WriteString(" [" + strings.Join(e.Columns, ", ") + "]")
	}
	if e.Cause != nil {
		builder.WriteString(": " + e.Cause.Error())
	}
	return builder.String()
}

func (e *ConstraintError) Unwrap() error { return e.Cause }

func (e *ConstraintError) Is(target error) bool { return target == ErrConstraint }

// These report the constraint class of an error produced anywhere in the ORM.
// They are named "...Violation" to stay clear of the predicate constructors
// in expr.go, where IsNull and IsNotNull build SQL rather than inspect errors.
func IsUniqueViolation(err error) bool     { return isConstraintKind(err, ConstraintUnique) }
func IsForeignKeyViolation(err error) bool { return isConstraintKind(err, ConstraintForeignKey) }
func IsNotNullViolation(err error) bool    { return isConstraintKind(err, ConstraintNotNull) }
func IsCheckViolation(err error) bool      { return isConstraintKind(err, ConstraintCheck) }

func isConstraintKind(err error, kind ConstraintKind) bool {
	var constraint *ConstraintError
	if errors.As(err, &constraint) {
		return constraint.Kind == kind
	}
	return false
}

// MappingError reports a column that could not be mapped onto a model field.
type MappingError struct {
	Model  string
	Column string
	Cause  error
}

func newMappingError(model, column string, cause error) *MappingError {
	return &MappingError{Model: model, Column: column, Cause: cause}
}

func (e *MappingError) Error() string {
	message := fmt.Sprintf("orm: cannot map column %q to %s", e.Column, e.Model)
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *MappingError) Unwrap() error { return e.Cause }

type UnsupportedFeatureError struct {
	Dialect string
	Feature string
}

func (e *UnsupportedFeatureError) Error() string {
	return fmt.Sprintf("orm: %s does not support %s", e.Dialect, e.Feature)
}

func (e *UnsupportedFeatureError) Is(target error) bool { return target == ErrUnsupported }

// ErrInvalidIdentifier reports a name that cannot be safely interpolated into
// SQL.
var ErrInvalidIdentifier = errors.New("orm: invalid identifier")

// CheckIdentifier rejects anything that is not a plain SQL identifier.
//
// Some names — savepoints above all — cannot be passed as bound parameters and
// must be written into the statement text, so they are validated rather than
// trusted.
func CheckIdentifier(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name is empty", ErrInvalidIdentifier)
	}
	for i, char := range name {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char == '_':
		case char >= '0' && char <= '9' && i > 0:
		default:
			return fmt.Errorf("%w: %q must be letters, digits or underscores", ErrInvalidIdentifier, name)
		}
	}
	return nil
}

func CheckRowsAffected(result interface{ RowsAffected() (int64, error) }) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNoRowsAffected
	}
	return nil
}
