package orm

// The naming and tag rules are exported so that code generators produce
// descriptors identical to what the runtime resolves. Call these rather than
// reimplementing them: a second copy that pluralises even slightly
// differently has the ORM querying "categorys" while the migration created
// "categories".

// ColumnName returns the column a struct field maps to by default.
func ColumnName(fieldName string) string { return snakeCase(fieldName) }

// TableNameFor returns the table a model type name maps to by default. It does
// not consult TableNamer, which only a concrete type can answer.
func TableNameFor(structName string) string {
	if structName == "" {
		return ""
	}
	return inflector.Plural(snakeCase(structName))
}

// PivotColumnFor returns the conventional pivot column for a model name.
func PivotColumnFor(structName string) string {
	return inflector.Singular(snakeCase(structName)) + "_id"
}

// TagInfo is the parsed form of an orm struct tag.
type TagInfo struct {
	Column        string
	PrimaryKey    bool
	AutoIncrement bool
	Skip          bool

	SoftDelete     bool
	AutoCreateTime bool
	AutoUpdateTime bool

	Relation        RelationKind
	RelationKey     string
	References      string
	PivotTable      string
	PivotForeignKey string
	PivotTargetKey  string
}

// ParseTag parses an orm struct tag value.
func ParseTag(value string) TagInfo {
	parsed := parseORMTag(value)
	return TagInfo{
		Column:          parsed.column,
		PrimaryKey:      parsed.primaryKey,
		AutoIncrement:   parsed.autoIncrement,
		Skip:            parsed.skip,
		SoftDelete:      parsed.softDelete,
		AutoCreateTime:  parsed.autoCreateTime,
		AutoUpdateTime:  parsed.autoUpdateTime,
		Relation:        parsed.relation,
		RelationKey:     parsed.relationKey,
		References:      parsed.references,
		PivotTable:      parsed.pivotTable,
		PivotForeignKey: parsed.pivotForeignKey,
		PivotTargetKey:  parsed.pivotTargetKey,
	}
}
