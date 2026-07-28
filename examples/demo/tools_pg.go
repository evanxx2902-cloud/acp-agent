package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var pgPool *sql.DB

func registerPGTools(s *server.MCPServer) {
	pgPool = connectPG()

	s.AddTool(mcp.NewTool("pg_query",
		mcp.WithDescription("Execute a SQL query against the PostgreSQL database and return results."),
		mcp.WithString("sql", mcp.Description("SQL query to execute"), mcp.Required()),
	), pgQuery)

	s.AddTool(mcp.NewTool("pg_list_tables",
		mcp.WithDescription("List all tables in the PostgreSQL database with their schemas."),
	), pgListTables)
}

func connectPG() *sql.DB {
	password, err := os.ReadFile("/run/secure/service")
	if err != nil {
		fmt.Fprintf(os.Stderr, "MCP: failed to read PG password: %v (pg tools disabled)\n", err)
		return nil
	}
	pass := strings.TrimSpace(string(password))

	dsn := fmt.Sprintf("user=pam password=%s host=/tmp port=5432 dbname=app sslmode=disable", pass)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "MCP: failed to open PG: %v (pg tools disabled)\n", err)
		return nil
	}

	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "MCP: PG ping failed: %v (pg tools disabled)\n", err)
		db.Close()
		return nil
	}

	return db
}

func pgQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if pgPool == nil {
		return mcp.NewToolResultError("PostgreSQL not available"), nil
	}
	query := req.GetString("sql", "")
	if query == "" {
		return mcp.NewToolResultError("sql is required"), nil
	}

	rows, err := pgPool.QueryContext(ctx, query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query failed: %v", err)), nil
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var lines []string
	lines = append(lines, strings.Join(cols, "\t"))

	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		rows.Scan(ptrs...)

		var row []string
		for _, v := range vals {
			row = append(row, fmt.Sprintf("%v", v))
		}
		lines = append(lines, strings.Join(row, "\t"))
	}

	if len(lines) <= 1 {
		return mcp.NewToolResultText("(empty result)"), nil
	}
	return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
}

func pgListTables(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if pgPool == nil {
		return mcp.NewToolResultError("PostgreSQL not available"), nil
	}
	rows, err := pgPool.QueryContext(ctx, `
		SELECT table_schema, table_name
		FROM information_schema.tables
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
		ORDER BY table_schema, table_name
	`)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list tables failed: %v", err)), nil
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var schema, name string
		rows.Scan(&schema, &name)
		lines = append(lines, fmt.Sprintf("%s.%s", schema, name))
	}
	if len(lines) == 0 {
		return mcp.NewToolResultText("(no tables)"), nil
	}
	return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
}
