package pq

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	sq "github.com/lann/squirrel"
	"github.com/lib/pq"
	"github.com/oursky/skygear/skydb"
)

// predicateSqlizerFactory is a factory for creating sqlizer for predicate
type predicateSqlizerFactory struct {
	db           *database
	primaryTable string
	joinedTables []joinedTable
	extraColumns map[string]skydb.FieldType
}

func (f *predicateSqlizerFactory) newPredicateSqlizer(predicate skydb.Predicate) (sq.Sqlizer, error) {
	if predicate.IsEmpty() {
		panic("no sqlizer can be created from an empty predicate")
	}

	if predicate.Operator == skydb.Functional {
		return f.newFunctionalPredicateSqlizer(predicate)
	}
	if predicate.Operator.IsCompound() {
		return f.newCompoundPredicateSqlizer(predicate)
	}
	if predicate.Operator == skydb.In {
		return &containsComparisonPredicateSqlizer{f.primaryTable, predicate}, nil
	}
	return &comparisonPredicateSqlizer{f.primaryTable, predicate}, nil
}

func (f *predicateSqlizerFactory) newCompoundPredicateSqlizer(p skydb.Predicate) (sq.Sqlizer, error) {
	switch p.Operator {
	default:
		err := fmt.Errorf("Compound operator `%v` is not supported.", p.Operator)
		return nil, err
	case skydb.And:
		and := make(sq.And, len(p.Children))
		for i, child := range p.Children {
			sqlizer, err := f.newPredicateSqlizer(child.(skydb.Predicate))
			if err != nil {
				return nil, err
			}
			and[i] = sqlizer
		}
		return and, nil
	case skydb.Or:
		or := make(sq.Or, len(p.Children))
		for i, child := range p.Children {
			sqlizer, err := f.newPredicateSqlizer(child.(skydb.Predicate))
			if err != nil {
				return nil, err
			}
			or[i] = sqlizer
		}
		return or, nil
	case skydb.Not:
		pred := p.Children[0].(skydb.Predicate)
		sqlizer, err := f.newPredicateSqlizer(pred)
		if err != nil {
			return nil, err
		}
		return NotSqlizer{sqlizer}, nil
	}
}

func (f *predicateSqlizerFactory) newFunctionalPredicateSqlizer(predicate skydb.Predicate) (sq.Sqlizer, error) {
	expr := predicate.Children[0].(skydb.Expression)
	if expr.Type != skydb.Function {
		panic("unexpected expression in functional predicate")
	}
	switch fn := expr.Value.(type) {
	case skydb.UserRelationFunc:
		return f.newUserRelationFunctionalPredicateSqlizer(fn)
	case skydb.UserDiscoverFunc:
		return f.newUserDiscoverFunctionalPredicateSqlizer(fn)
	default:
		panic("the specified function cannot be used as a functional predicate")
	}
}

func (f *predicateSqlizerFactory) newUserRelationFunctionalPredicateSqlizer(fn skydb.UserRelationFunc) (sq.Sqlizer, error) {
	table := fn.RelationName
	direction := fn.RelationDirection
	if direction == "" {
		direction = "outward"
	}
	primaryColumn := fn.KeyPath
	if primaryColumn == "_owner" || primaryColumn == "" {
		primaryColumn = "_owner_id"
	}

	var outwardAlias, inwardAlias string
	if direction == "outward" || direction == "mutual" {
		outwardAlias = f.createLeftJoin(table, primaryColumn, "right_id")
	}
	if direction == "inward" || direction == "mutual" {
		inwardAlias = f.createLeftJoin(table, primaryColumn, "left_id")
	}

	return userRelationPredicateSqlizer{
		outwardAlias: outwardAlias,
		inwardAlias:  inwardAlias,
		user:         fn.User,
	}, nil
}

func (f *predicateSqlizerFactory) newUserDiscoverFunctionalPredicateSqlizer(fn skydb.UserDiscoverFunc) (sq.Sqlizer, error) {
	if f.db.UserRecordType() != f.primaryTable {
		return nil, fmt.Errorf("user discover predicate can only be used on user record")
	}

	sqlizers := []sq.Sqlizer{}
	// Only email is supported at the moment
	sqlizers = append(sqlizers, &containsComparisonPredicateSqlizer{
		f.createLeftJoin("_user", "_id", "id"),
		skydb.Predicate{
			Operator: skydb.In,
			Children: []interface{}{
				skydb.Expression{
					Type:  skydb.KeyPath,
					Value: "email",
				},
				skydb.Expression{
					Type:  skydb.Literal,
					Value: fn.ArgsByName("email"),
				},
			},
		},
	})

	f.addExtraColumn("_transient__email", skydb.TypeString, skydb.Expression{
		Type:  skydb.Function,
		Value: skydb.UserDataFunc{"email"},
	})

	f.addExtraColumn("_transient__username", skydb.TypeString, skydb.Expression{
		Type:  skydb.Function,
		Value: skydb.UserDataFunc{"username"},
	})

	return sqlizers[0], nil
}

func (f *predicateSqlizerFactory) newAccessControlSqlizer(user skydb.UserInfo, aclLevel skydb.ACLLevel) (sq.Sqlizer, error) {
	return &accessPredicateSqlizer{
		f.db.ID(),
		user,
		aclLevel,
	}, nil
}

// createLeftJoin create an alias of a table to be joined to the primary table
// and return the alias for the joined table
func (f *predicateSqlizerFactory) createLeftJoin(secondaryTable string, primaryColumn string, secondaryColumn string) string {
	newAlias := joinedTable{secondaryTable, primaryColumn, secondaryColumn}
	for i, alias := range f.joinedTables {
		if alias.equal(newAlias) {
			return f.aliasName(secondaryTable, i)
		}
	}

	f.joinedTables = append(f.joinedTables, newAlias)
	return f.aliasName(secondaryTable, len(f.joinedTables)-1)
}

func (f *predicateSqlizerFactory) aliasName(secondaryTable string, indexInJoinedTables int) string {
	// The _user table always have the same alias name for
	// getting user info in user discovery
	if secondaryTable == "_user" {
		return "_user"
	}
	return fmt.Sprintf("_t%d", indexInJoinedTables)
}

// addJoinsToSelectBuilder add join clauses to a SelectBuilder
func (f *predicateSqlizerFactory) addJoinsToSelectBuilder(q sq.SelectBuilder) sq.SelectBuilder {
	for i, alias := range f.joinedTables {
		aliasName := f.aliasName(alias.secondaryTable, i)
		joinClause := fmt.Sprintf("%s AS %s ON %s = %s",
			f.db.tableName(alias.secondaryTable), pq.QuoteIdentifier(aliasName),
			fullQuoteIdentifier(f.primaryTable, alias.primaryColumn),
			fullQuoteIdentifier(aliasName, alias.secondaryColumn))
		q = q.LeftJoin(joinClause)
	}

	if len(f.joinedTables) > 0 {
		q = q.Distinct()
	}
	return q
}

func (f *predicateSqlizerFactory) addExtraColumn(key string, fieldType skydb.DataType, expr skydb.Expression) {
	if f.extraColumns == nil {
		f.extraColumns = map[string]skydb.FieldType{}
	}
	f.extraColumns[key] = skydb.FieldType{
		Type:       fieldType,
		Expression: expr,
	}
}

func (f *predicateSqlizerFactory) updateTypemap(typemap skydb.RecordSchema) skydb.RecordSchema {
	for key, field := range f.extraColumns {
		typemap[key] = field
	}
	return typemap
}

func newPredicateSqlizerFactory(db *database, primaryTable string) *predicateSqlizerFactory {
	return &predicateSqlizerFactory{
		db:           db,
		primaryTable: primaryTable,
		joinedTables: []joinedTable{},
	}
}

// accessPredicateSqlizer build the json matching expression base on user's
// role, the builded express will filter out record which user is not accessible.
//
// The sql for record accessible by user rickmak
// `_access @> '[{"user_id":"rickmak"}]'`
//
// Record accessible by user with admin role
// `_access @> '[{"role":"admin"}]'`
//
// Record accessible by user rickmak or admin role
// `_access @> '[{"role":"rickmak"}]' OR _access @> '[{"role":"admin"}]'`¬
type accessPredicateSqlizer struct {
	databaseID string
	user       skydb.UserInfo
	level      skydb.ACLLevel
}

func (p accessPredicateSqlizer) ToSql() (sql string, args []interface{}, err error) {
	if p.databaseID == "" {
		sql = ``
	}
	if p.user.ID != "" {
		var b bytes.Buffer
		b.WriteString(`(`)
		for _, role := range p.user.Roles {
			escapedRole, err := json.Marshal(role)
			if err != nil {
				panic("unexpected serialze error on role")
			}
			b.WriteString(`_access @> '[{"role":`)
			b.Write(escapedRole)
			b.WriteString(`}]' OR `)
		}
		b.WriteString(`_access @> '[{"user_id":`)
		escapedID, _ := json.Marshal(p.user.ID)
		if err != nil {
			panic("unexpected serialze error on user_id")
		}
		b.Write(escapedID)
		b.WriteString(`}]' OR _access IS NULL OR _owner_id = ?)`)
		sql = b.String()
		args = []interface{}{p.user.ID}
	}

	err = nil
	return
}

type userRelationPredicateSqlizer struct {
	outwardAlias string
	inwardAlias  string
	user         string
}

func (p userRelationPredicateSqlizer) ToSql() (sql string, args []interface{}, err error) {
	if p.outwardAlias != "" && p.inwardAlias != "" {
		sql = fmt.Sprintf("%s = %s AND %s = ?",
			fullQuoteIdentifier(p.outwardAlias, "left_id"),
			fullQuoteIdentifier(p.inwardAlias, "right_id"),
			fullQuoteIdentifier(p.outwardAlias, "left_id"))
	} else if p.outwardAlias != "" {
		sql = fmt.Sprintf("%s = ?",
			fullQuoteIdentifier(p.outwardAlias, "left_id"))
	} else if p.inwardAlias != "" {
		sql = fmt.Sprintf("%s = ?",
			fullQuoteIdentifier(p.inwardAlias, "right_id"))
	} else {
		panic("unexpected value in sqlizer")
	}
	args = []interface{}{p.user}
	err = nil
	return
}

type containsComparisonPredicateSqlizer struct {
	alias string
	skydb.Predicate
}

func (p *containsComparisonPredicateSqlizer) ToSql() (sql string, args []interface{}, err error) {
	var buffer bytes.Buffer
	lhs := expressionSqlizer{p.alias, p.Children[0].(skydb.Expression)}
	rhs := expressionSqlizer{p.alias, p.Children[1].(skydb.Expression)}

	if lhs.Type == skydb.Literal && rhs.Type == skydb.KeyPath {
		buffer.WriteString(`jsonb_exists(`)

		sqlOperand, opArgs, err := rhs.ToSql()
		if err != nil {
			return "", nil, err
		}
		buffer.WriteString(sqlOperand)
		args = append(args, opArgs...)

		buffer.WriteString(`, `)

		sqlOperand, opArgs, err = lhs.ToSql()
		if err != nil {
			return "", nil, err
		}
		buffer.WriteString(sqlOperand)
		args = append(args, opArgs...)

		buffer.WriteString(`)`)

		sql = buffer.String()
		return sql, args, err
	} else if lhs.Type == skydb.KeyPath && rhs.Type == skydb.Literal {
		sqlOperand, opArgs, err := lhs.ToSql()
		if err != nil {
			return "", nil, err
		}
		buffer.WriteString(sqlOperand)
		args = append(args, opArgs...)

		buffer.WriteString(` IN `)

		sqlOperand, opArgs, err = rhs.ToSql()
		if err != nil {
			return "", nil, err
		}
		buffer.WriteString(sqlOperand)
		args = append(args, opArgs...)

		sql = buffer.String()
		return sql, args, err
	} else {
		panic("malformed query")
	}
}

type comparisonPredicateSqlizer struct {
	alias string
	skydb.Predicate
}

func (p *comparisonPredicateSqlizer) ToSql() (sql string, args []interface{}, err error) {
	args = []interface{}{}
	if p.Operator.IsBinary() {
		var buffer bytes.Buffer
		lhs := expressionSqlizer{p.alias, p.Children[0].(skydb.Expression)}
		rhs := expressionSqlizer{p.alias, p.Children[1].(skydb.Expression)}

		sqlOperand, opArgs, err := lhs.ToSql()
		if err != nil {
			return "", nil, err
		}
		buffer.WriteString(sqlOperand)
		args = append(args, opArgs...)

		switch p.Operator {
		default:
			err = fmt.Errorf("Comparison operator `%v` is not supported.", p.Operator)
			return sql, args, err
		case skydb.Equal:
			buffer.WriteString(`=`)
		case skydb.GreaterThan:
			buffer.WriteString(`>`)
		case skydb.LessThan:
			buffer.WriteString(`<`)
		case skydb.GreaterThanOrEqual:
			buffer.WriteString(`>=`)
		case skydb.LessThanOrEqual:
			buffer.WriteString(`<=`)
		case skydb.NotEqual:
			buffer.WriteString(`<>`)
		case skydb.Like:
			buffer.WriteString(` LIKE `)
		case skydb.ILike:
			buffer.WriteString(` ILIKE `)
		}

		sqlOperand, opArgs, err = rhs.ToSql()
		if err != nil {
			return "", nil, err
		}
		buffer.WriteString(sqlOperand)
		args = append(args, opArgs...)

		sql = buffer.String()
	} else {
		err = fmt.Errorf("Comparison operator `%v` is not supported.", p.Operator)
	}

	return
}

type expressionSqlizer struct {
	alias string
	skydb.Expression
}

func (expr *expressionSqlizer) ToSql() (sql string, args []interface{}, err error) {
	switch expr.Type {
	case skydb.KeyPath:
		sql = fullQuoteIdentifier(expr.alias, expr.Value.(string))
		args = []interface{}{}
	case skydb.Function:
		sql, args = funcToSQLOperand(expr.alias, expr.Value.(skydb.Func))
	default:
		sql, args = literalToSQLOperand(expr.Value)
	}
	return
}

func funcToSQLOperand(alias string, fun skydb.Func) (string, []interface{}) {
	switch f := fun.(type) {
	case skydb.DistanceFunc:
		sql := fmt.Sprintf("ST_Distance_Sphere(%s, ST_MakePoint(?, ?))",
			fullQuoteIdentifier(alias, f.Field))
		args := []interface{}{f.Location.Lng(), f.Location.Lat()}
		return sql, args
	case skydb.CountFunc:
		var sql string
		if f.OverallRecords {
			sql = fmt.Sprintf("COUNT(*) OVER()")
		} else {
			sql = fmt.Sprintf("COUNT(*)")
		}
		args := []interface{}{}
		return sql, args
	case skydb.UserDataFunc:
		return fmt.Sprintf("_user.%s", f.DataName), []interface{}{}
	default:
		panic(fmt.Errorf("got unrecgonized skydb.Func = %T", fun))
	}
}

func literalToSQLOperand(literal interface{}) (string, []interface{}) {
	// Array detection is borrowed from squirrel's expr.go
	switch literalValue := literal.(type) {
	case []interface{}:
		argCount := len(literalValue)
		if argCount > 0 {
			args := make([]interface{}, len(literalValue))
			for i, val := range literalValue {
				args[i] = literalToSQLValue(val)
			}
			return "(" + sq.Placeholders(len(literalValue)) + ")", args
		}

		// NOTE(limouren): trick to make `field IN (...)` work for empty list
		// NULL field won't match the condition since NULL == NULL is falsy,
		// which renders `field IN(NULL)` equivalent to FALSE
		return "(NULL)", nil
	default:
		return sq.Placeholders(1), []interface{}{literalToSQLValue(literal)}
	}
}

func literalToSQLValue(value interface{}) interface{} {
	switch v := value.(type) {
	case skydb.Reference:
		return v.ID.Key
	default:
		return value
	}
}

func sortOrderBySQL(alias string, sort skydb.Sort) (string, error) {
	var expr string

	switch {
	case sort.KeyPath != "":
		expr = fullQuoteIdentifier(alias, sort.KeyPath)
	case sort.Func != nil:
		var err error
		expr, err = funcOrderBySQL(alias, sort.Func)
		if err != nil {
			return "", err
		}
	default:
		return "", errors.New("invalid Sort: specify either KeyPath or Func")
	}

	order, err := sortOrderOrderBySQL(sort.Order)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(expr + " " + order), nil
}

// due to sq not being able to pass args in OrderBy, we can't re-use funcToSQLOperand
func funcOrderBySQL(alias string, fun skydb.Func) (string, error) {
	switch f := fun.(type) {
	case skydb.DistanceFunc:
		sql := fmt.Sprintf(
			"ST_Distance_Sphere(%s, ST_MakePoint(%f, %f))",
			fullQuoteIdentifier(alias, f.Field),
			f.Location.Lng(),
			f.Location.Lat(),
		)
		return sql, nil
	default:
		return "", fmt.Errorf("got unrecgonized skydb.Func = %T", fun)
	}
}

func sortOrderOrderBySQL(order skydb.SortOrder) (string, error) {
	switch order {
	case skydb.Asc:
		return "ASC", nil
	case skydb.Desc:
		return "DESC", nil
	default:
		return "", fmt.Errorf("unknown sort order = %v", order)
	}
}

func pqDataType(dataType skydb.DataType) string {
	switch dataType {
	default:
		panic(fmt.Sprintf("Unsupported dataType = %s", dataType))
	case skydb.TypeString, skydb.TypeAsset, skydb.TypeReference:
		return TypeString
	case skydb.TypeNumber:
		return TypeNumber
	case skydb.TypeInteger:
		return TypeInteger
	case skydb.TypeDateTime:
		return TypeTimestamp
	case skydb.TypeBoolean:
		return TypeBoolean
	case skydb.TypeJSON:
		return TypeJSON
	case skydb.TypeLocation:
		return TypeLocation
	case skydb.TypeSequence:
		return TypeSerial
	}
}

func fullQuoteIdentifier(aliasName string, columnName string) string {
	return pq.QuoteIdentifier(aliasName) + "." + pq.QuoteIdentifier(columnName)
}

// NotSqlizer generates SQL condition that negates a boolean condition
type NotSqlizer struct {
	Predicate sq.Sqlizer
}

// ToSql generates SQL for NotSqlizer
func (s NotSqlizer) ToSql() (sql string, args []interface{}, err error) {
	sql, args, err = s.Predicate.ToSql()
	if err != nil {
		sql = fmt.Sprintf("NOT (%s)", sql)
	}
	return
}

// joinedTable represents a specification for table join
type joinedTable struct {
	secondaryTable  string
	primaryColumn   string
	secondaryColumn string
}

// equal compares whether two specifications of table join are equal
func (a joinedTable) equal(b joinedTable) bool {
	return a.secondaryTable == b.secondaryTable && a.primaryColumn == b.primaryColumn && a.secondaryColumn == b.secondaryColumn
}
