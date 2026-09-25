package genfixture

import (
	"reflect"
	"testing"

	"github.com/lemmego/orm"
)

// The generator and the runtime must agree exactly. If they ever drift, a
// query built from descriptors would reference columns the mapper does not
// know about — the same class of bug as the old "categorys" mismatch.
func TestDescriptorsMatchRuntimeMetadata(t *testing.T) {
	cases := []struct {
		name        string
		table       string
		descriptors any
		info        *orm.ModelInfo
	}{
		{"User", UserTable, UserFields, mustInfo[User](t)},
		{"Post", PostTable, PostFields, mustInfo[Post](t)},
		{"Profile", ProfileTable, ProfileFields, mustInfo[Profile](t)},
		{"Tag", TagTable, TagFields, mustInfo[Tag](t)},
		{"Category", CategoryTable, CategoryFields, mustInfo[Category](t)},
		{"Account", AccountTable, AccountFields, mustInfo[Account](t)},
		{"LegacyUser", LegacyUserTable, LegacyUserFields, mustInfo[LegacyUser](t)},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.info.Table != testCase.table {
				t.Fatalf("table constant %q does not match the runtime table %q",
					testCase.table, testCase.info.Table)
			}

			value := reflect.ValueOf(testCase.descriptors)
			if value.NumField() != len(testCase.info.Fields) {
				t.Fatalf("generated %d descriptors but the runtime maps %d columns",
					value.NumField(), len(testCase.info.Fields))
			}

			for i := range value.NumField() {
				fieldName := value.Type().Field(i).Name
				column := value.Field(i).MethodByName("Column").Call(nil)[0].String()

				runtimeField, ok := testCase.info.ByName[fieldName]
				if !ok {
					t.Fatalf("descriptor %s has no matching runtime field", fieldName)
				}
				if runtimeField.Column != column {
					t.Fatalf("descriptor %s maps to %q but the runtime maps it to %q",
						fieldName, column, runtimeField.Column)
				}
			}
		})
	}
}

// A descriptor predicate and the equivalent string predicate must compile to
// byte-identical SQL.
func TestDescriptorsMatchStringPredicates(t *testing.T) {
	db := orm.Open(nil, orm.Postgres())

	typedQuery, typedArgs := db.Model[User]().
		Where(UserFields.Email.Eq("a@b.c")).
		Where(UserFields.Age.Gte(18)).
		OrderBy(UserFields.CreatedAt.Desc()).
		SQL()

	stringQuery, stringArgs := db.Model[User]().
		Where(orm.Eq("email_address", "a@b.c")).
		Where(orm.Gte("age", 18)).
		OrderBy(orm.Desc("created_at")).
		SQL()

	if typedQuery != stringQuery {
		t.Fatalf("SQL diverged:\n  typed  %s\n  string %s", typedQuery, stringQuery)
	}
	if !reflect.DeepEqual(typedArgs, stringArgs) {
		t.Fatalf("args diverged: %#v vs %#v", typedArgs, stringArgs)
	}
}

// Relations, orm:"-" fields and unexported fields must not become descriptors.
func TestNonColumnFieldsAreExcluded(t *testing.T) {
	value := reflect.TypeOf(UserFields)
	for _, excluded := range []string{"Posts", "Profile", "Tags", "Secret"} {
		if _, found := value.FieldByName(excluded); found {
			t.Fatalf("%s must not have a descriptor", excluded)
		}
	}
	// Embedded columns must be present.
	for _, included := range []string{"CreatedAt", "UpdatedAt"} {
		if _, found := value.FieldByName(included); !found {
			t.Fatalf("embedded column %s is missing a descriptor", included)
		}
	}
}

func mustInfo[T any](t *testing.T) *orm.ModelInfo {
	t.Helper()
	info, err := orm.ModelInfoFor[T]()
	if err != nil {
		t.Fatal(err)
	}
	return info
}
