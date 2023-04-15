package sql

import (
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/persistence/sql/sqlplugin/mysql"
)

type (
	StatementBuilder interface {
		build() (string, []any)
	}

	statementBuilder struct {
		namespaceID namespace.ID
		queryString string
	}

	selectStatementBuilder struct {
		statementBuilder
		supportsOrderBy bool
		pageSize        int
		token           *pageToken
	}

	countStatementBuilder struct {
		statementBuilder
	}

	SelectStatementBuilderOption func(*selectStatementBuilder)
)

func newStatementBuilder(namespace namespace.ID, queryString string) *statementBuilder {
	return &statementBuilder{queryString: queryString}
}

func NewSelectStatementBuilder(pluginName string, namespaceID namespace.ID, queryString string, opts ...SelectStatementBuilderOption) StatementBuilder {
	const (
		defaultSupportsOrderBy = false
		defaultPageSize        = 1000
		defaultPageToken       = 0
	)

	ssb := selectStatementBuilder{
		statementBuilder: *newStatementBuilder(namespaceID, queryString),
		supportsOrderBy:  defaultSupportsOrderBy,
	}

	for _, opt := range opts {
		opt(&ssb)
	}

	switch pluginName {
	case mysql.PluginNameV8:
		return newMysqlSelectStatementBuilder(ssb)
	//case postgresql.PluginNameV12:
	//case sqlite.PluginName:
	default:
		return nil
	}

}

func WithOrderBy() SelectStatementBuilderOption {
	return func(ssb *selectStatementBuilder) {
		ssb.supportsOrderBy = true
	}
}

func WithPageSize(pageSize int) SelectStatementBuilderOption {
	return func(ssb *selectStatementBuilder) {
		ssb.pageSize = pageSize
	}
}

func WithToken(token *pageToken) SelectStatementBuilderOption {
	return func(ssb *selectStatementBuilder) {
		ssb.token = token
	}
}
