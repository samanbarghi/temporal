package sql

import (
	"fmt"
	"strings"

	"github.com/xwb1989/sqlparser"
	"go.temporal.io/server/common/persistence/sql/sqlplugin"
	"go.temporal.io/server/common/searchattribute"
)

type (
	mysqlSelectStatementBuilder struct {
		selectStatementBuilder
	}
)

func newMysqlSelectStatementBuilder(ssb selectStatementBuilder) *mysqlSelectStatementBuilder {
	return &mysqlSelectStatementBuilder{selectStatementBuilder: ssb}
}
func (m *mysqlSelectStatementBuilder) build() (string, []any) {

	var whereClauses []string
	var queryArgs []any

	whereClauses = append(
		whereClauses,
		fmt.Sprintf("%s = ?", searchattribute.GetSqlDbColName(searchattribute.NamespaceID)),
	)
	queryArgs = append(queryArgs, m.namespaceID.String())

	if len(m.queryString) > 0 {
		whereClauses = append(whereClauses, m.queryString)
	}

	if m.token != nil {
		whereClauses = append(
			whereClauses,
			fmt.Sprintf(
				"((%s = ? AND %s = ? AND %s > ?) OR (%s = ? AND %s < ?) OR %s < ?)",
				sqlparser.String(c.getCoalesceCloseTimeExpr()),
				searchattribute.GetSqlDbColName(searchattribute.StartTime),
				searchattribute.GetSqlDbColName(searchattribute.RunID),
				sqlparser.String(c.getCoalesceCloseTimeExpr()),
				searchattribute.GetSqlDbColName(searchattribute.StartTime),
				sqlparser.String(c.getCoalesceCloseTimeExpr()),
			),
		)
		queryArgs = append(
			queryArgs,
			m.token.CloseTime,
			m.token.StartTime,
			m.token.RunID,
			m.token.CloseTime,
			m.token.StartTime,
			m.token.CloseTime,
		)
	}

	queryArgs = append(queryArgs, m.pageSize)

	return fmt.Sprintf(
		`SELECT %s
		FROM executions_visibility ev
		LEFT JOIN custom_search_attributes
		USING (%s, %s)
		WHERE %s
		ORDER BY %s DESC, %s DESC, %s
		LIMIT ?`,
		strings.Join(addPrefix("ev.", sqlplugin.DbFields), ", "),
		searchattribute.GetSqlDbColName(searchattribute.NamespaceID),
		searchattribute.GetSqlDbColName(searchattribute.RunID),
		strings.Join(whereClauses, " AND "),
		sqlparser.String(c.getCoalesceCloseTimeExpr()),
		searchattribute.GetSqlDbColName(searchattribute.StartTime),
		searchattribute.GetSqlDbColName(searchattribute.RunID),
	), queryArgs
}
