// Package ormgen generates typed field descriptors for ORM models.
//
// It calls the ORM's own naming and tag rules rather than reimplementing them,
// so a generated descriptor always resolves to the same column the runtime
// does. It lives in its own package so that importing the ORM does not drag in
// go/ast.
package ormgen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/lemmego/orm"
)

const (
	// OrmImportPath is the import added to every generated file.
	OrmImportPath = "github.com/lemmego/orm"

	// OutputFileName is the default name of the generated file.
	OutputFileName = "orm_fields.go"
)

// Options controls one generation run.
type Options struct {
	Dir     string
	Only    []string // when set, only these type names are generated
	All     bool     // generate for every exported struct, tagged or not
	Package string   // overrides the detected package name
}

// model is one struct we will emit descriptors for.
type model struct {
	Name      string
	Table     string
	TableFrom string // "TableName" when read off a TableNamer method
	Fields    []field
}

type field struct {
	Name   string
	Column string
	Type   string // the Value type parameter
}

// Generate parses a directory of Go source and returns the formatted contents
// of the descriptor file.
func Generate(options Options) ([]byte, error) {
	fileSet := token.NewFileSet()
	packages, err := parser.ParseDir(fileSet, options.Dir, func(info os.FileInfo) bool {
		name := info.Name()
		return !strings.HasSuffix(name, "_test.go") && name != OutputFileName
	}, 0)
	if err != nil {
		return nil, err
	}

	pkg, packageName, err := selectPackage(packages)
	if err != nil {
		return nil, err
	}
	if options.Package != "" {
		packageName = options.Package
	}

	structs := collectStructs(pkg)
	tableNames := collectTableNames(pkg)

	only := make(map[string]bool, len(options.Only))
	for _, name := range options.Only {
		only[name] = true
	}

	var models []model
	imports := map[string]string{}

	names := make([]string, 0, len(structs))
	for name := range structs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		declared := structs[name]
		if len(only) > 0 && !only[name] {
			continue
		}
		if len(only) == 0 && !options.All && !hasMappingTag(declared.node, structs) {
			continue
		}
		if !ast.IsExported(name) {
			continue
		}

		built, err := buildModel(name, declared, structs, tableNames, imports)
		if err != nil {
			return nil, err
		}
		if len(built.Fields) == 0 {
			continue
		}
		models = append(models, built)
	}

	if len(models) == 0 {
		return nil, fmt.Errorf("ormgen: no models found in %s", options.Dir)
	}
	return render(packageName, models, imports)
}

// declaredStruct pairs a struct type with the file it came from, since import
// qualifiers are per-file.
type declaredStruct struct {
	node    *ast.StructType
	imports map[string]string // qualifier -> import path
}

func selectPackage(packages map[string]*ast.Package) (*ast.Package, string, error) {
	names := make([]string, 0, len(packages))
	for name := range packages {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		// Skip the external test package if one is present.
		if strings.HasSuffix(name, "_test") {
			continue
		}
		return packages[name], name, nil
	}
	return nil, "", fmt.Errorf("ormgen: no Go package found")
}

func collectStructs(pkg *ast.Package) map[string]declaredStruct {
	structs := make(map[string]declaredStruct)
	for _, file := range pkg.Files {
		fileImports := importQualifiers(file)
		for _, decl := range file.Decls {
			general, ok := decl.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				structs[typeSpec.Name.Name] = declaredStruct{node: structType, imports: fileImports}
			}
		}
	}
	return structs
}

// collectTableNames reads literal TableName() overrides so the generated table
// constant matches what the runtime will actually resolve.
func collectTableNames(pkg *ast.Package) map[string]string {
	tables := make(map[string]string)
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Name.Name != "TableName" || function.Recv == nil || len(function.Recv.List) == 0 {
				continue
			}
			receiver := receiverTypeName(function.Recv.List[0].Type)
			if receiver == "" || function.Body == nil || len(function.Body.List) != 1 {
				continue
			}
			ret, ok := function.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				continue
			}
			literal, ok := ret.Results[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				continue
			}
			if value, err := strconv.Unquote(literal.Value); err == nil {
				tables[receiver] = value
			}
		}
	}
	return tables
}

func receiverTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(typed.X)
	case *ast.Ident:
		return typed.Name
	}
	return ""
}

func importQualifiers(file *ast.File) map[string]string {
	qualifiers := make(map[string]string)
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imported.Name != nil {
			name = imported.Name.Name
		}
		qualifiers[name] = path
	}
	return qualifiers
}

// hasMappingTag reports whether a struct carries any orm or db tag, which is
// the signal that it is a persisted model rather than an ordinary struct.
func hasMappingTag(structType *ast.StructType, structs map[string]declaredStruct) bool {
	for _, item := range structType.Fields.List {
		if item.Tag != nil {
			tag := reflectTag(item.Tag.Value)
			if _, ok := lookupTag(tag, "orm"); ok {
				return true
			}
			if _, ok := lookupTag(tag, "db"); ok {
				return true
			}
		}
		// An embedded model carries the signal for its container.
		if len(item.Names) == 0 {
			if name := receiverTypeName(item.Type); name != "" {
				if embedded, ok := structs[name]; ok && hasMappingTag(embedded.node, structs) {
					return true
				}
			}
		}
	}
	return false
}

func buildModel(name string, declared declaredStruct, structs map[string]declaredStruct, tables map[string]string, imports map[string]string) (model, error) {
	built := model{Name: name, Table: orm.TableNameFor(name)}
	if table, ok := tables[name]; ok {
		built.Table = table
		built.TableFrom = "TableName"
	}

	seen := make(map[string]bool)
	if err := appendFields(&built, declared, structs, imports, seen, 0); err != nil {
		return model{}, err
	}
	return built, nil
}

// appendFields mirrors the runtime's field collection, including recursion
// into embedded structs declared in the same package.
func appendFields(built *model, declared declaredStruct, structs map[string]declaredStruct, imports map[string]string, seen map[string]bool, depth int) error {
	if depth > 8 {
		return fmt.Errorf("ormgen: embedded structs nested too deeply in %s", built.Name)
	}

	for _, item := range declared.node.Fields.List {
		var tagValue string
		if item.Tag != nil {
			tagValue = ormTagValue(reflectTag(item.Tag.Value))
		}
		tag := orm.ParseTag(tagValue)
		if tag.Skip || tag.Relation != orm.RelationNone {
			continue
		}

		// Embedded struct: inline its fields, as the runtime does.
		if len(item.Names) == 0 {
			embeddedName := receiverTypeName(item.Type)
			embedded, ok := structs[embeddedName]
			if !ok {
				// Declared in another package; its fields are not visible here.
				continue
			}
			if err := appendFields(built, embedded, structs, imports, seen, depth+1); err != nil {
				return err
			}
			continue
		}

		for _, ident := range item.Names {
			if !ast.IsExported(ident.Name) {
				continue
			}
			if !isColumnExpr(item.Type, structs) {
				continue
			}

			column := tag.Column
			if column == "" {
				column = orm.ColumnName(ident.Name)
			}
			if seen[column] {
				continue
			}
			seen[column] = true

			valueType := renderType(baseType(item.Type), declared.imports, imports)
			built.Fields = append(built.Fields, field{
				Name:   ident.Name,
				Column: column,
				Type:   valueType,
			})
		}
	}
	return nil
}

// reflectTag strips the backquotes or quotes around a struct tag literal.
func reflectTag(literal string) string {
	if unquoted, err := strconv.Unquote(literal); err == nil {
		return unquoted
	}
	return strings.Trim(literal, "`")
}

// ormTagValue applies the same orm-then-db precedence the runtime uses.
func ormTagValue(tag string) string {
	if value, ok := lookupTag(tag, "orm"); ok {
		return value
	}
	if value, ok := lookupTag(tag, "db"); ok {
		if value == "-" {
			return "-"
		}
		name, _, _ := strings.Cut(value, ",")
		if name == "" {
			return ""
		}
		return "column:" + name
	}
	return ""
}

// lookupTag reads one key out of a struct tag, matching reflect.StructTag.
func lookupTag(tag, key string) (string, bool) {
	return reflect.StructTag(tag).Lookup(key)
}

func baseType(expr ast.Expr) ast.Expr {
	if star, ok := expr.(*ast.StarExpr); ok {
		return baseType(star.X)
	}
	return expr
}

// isColumnExpr is the syntactic counterpart of the runtime's isColumnType.
func isColumnExpr(expr ast.Expr, structs map[string]declaredStruct) bool {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return isColumnExpr(typed.X, structs)

	case *ast.Ident:
		switch typed.Name {
		case "bool", "string",
			"int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
			"float32", "float64", "byte", "rune":
			return true
		}
		// A named type in this package is a column only if it is not a struct.
		_, isStruct := structs[typed.Name]
		return !isStruct

	case *ast.SelectorExpr:
		// time.Time, sql.NullString and friends round-trip through one column.
		return true

	case *ast.ArrayType:
		ident, ok := typed.Elt.(*ast.Ident)
		return ok && (ident.Name == "byte" || ident.Name == "uint8")
	}
	return false
}

// renderType prints a type expression and records any package qualifier it
// uses, so the generated file imports exactly what it needs.
func renderType(expr ast.Expr, fileImports map[string]string, imports map[string]string) string {
	ast.Inspect(expr, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		if path, found := fileImports[ident.Name]; found {
			imports[ident.Name] = path
		}
		return true
	})
	return types.ExprString(expr)
}

func render(packageName string, models []model, imports map[string]string) ([]byte, error) {
	var buffer bytes.Buffer

	fmt.Fprintf(&buffer, "// Code generated by ormgen. DO NOT EDIT.\n\n")
	fmt.Fprintf(&buffer, "package %s\n\n", packageName)

	qualifiers := make([]string, 0, len(imports))
	for qualifier := range imports {
		qualifiers = append(qualifiers, qualifier)
	}
	sort.Strings(qualifiers)

	buffer.WriteString("import (\n")
	for _, qualifier := range qualifiers {
		path := imports[qualifier]
		// Only spell out the alias when it differs from the package's own name.
		if path[strings.LastIndex(path, "/")+1:] == qualifier {
			fmt.Fprintf(&buffer, "\t%q\n", path)
			continue
		}
		fmt.Fprintf(&buffer, "\t%s %q\n", qualifier, path)
	}
	if len(qualifiers) > 0 {
		buffer.WriteString("\n")
	}
	fmt.Fprintf(&buffer, "\t%q\n)\n\n", OrmImportPath)

	for _, built := range models {
		note := "the conventional table name"
		if built.TableFrom != "" {
			note = "the name declared by TableName()"
		}
		fmt.Fprintf(&buffer, "// %sTable is %s for %s.\n", built.Name, note, built.Name)
		fmt.Fprintf(&buffer, "const %sTable = %q\n\n", built.Name, built.Table)

		fmt.Fprintf(&buffer, "// %sFields holds typed column descriptors for %s.\n", built.Name, built.Name)
		fmt.Fprintf(&buffer, "var %sFields = struct {\n", built.Name)
		for _, item := range built.Fields {
			fmt.Fprintf(&buffer, "\t%s orm.Field[%s, %s]\n", item.Name, built.Name, item.Type)
		}
		buffer.WriteString("}{\n")
		for _, item := range built.Fields {
			fmt.Fprintf(&buffer, "\t%s: orm.NewField[%s, %s](%q),\n", item.Name, built.Name, item.Type, item.Column)
		}
		buffer.WriteString("}\n\n")
	}

	formatted, err := format.Source(buffer.Bytes())
	if err != nil {
		return nil, fmt.Errorf("ormgen: generated code did not parse: %w\n%s", err, buffer.String())
	}
	return formatted, nil
}
