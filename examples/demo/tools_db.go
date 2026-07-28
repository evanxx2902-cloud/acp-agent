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

var dbPool *sql.DB

func registerDBTools(s *server.MCPServer) {
	dbPool = connectDB()

	s.AddTool(mcp.NewTool("query_database",
		mcp.WithDescription("Execute a SQL query against the PostgreSQL database and return results."),
		mcp.WithString("sql", mcp.Description("SQL query to execute"), mcp.Required()),
	), dbQuery)

	s.AddTool(mcp.NewTool("list_database_tables",
		mcp.WithDescription("List all tables in the PostgreSQL database with their schemas."),
	), dbListTables)
}

func connectDB() *sql.DB {
	password, err := os.ReadFile("/run/secure/service")
	if err != nil {
		fmt.Fprintf(os.Stderr, "MCP: failed to read DB password: %v (db tools disabled)\n", err)
		return nil
	}
	pass := strings.TrimSpace(string(password))

	dsn := fmt.Sprintf("user=postgres password=%s dbname=app sslmode=disable connect_timeout=5", pass)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "MCP: failed to open DB: %v (db tools disabled)\n", err)
		return nil
	}

	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "MCP: DB ping failed: %v (db tools disabled)\n", err)
		db.Close()
		return nil
	}

	return db
}

func dbQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if dbPool == nil {
		return mcp.NewToolResultError("database not available"), nil
	}
	query := req.GetString("sql", "")
	if query == "" {
		return mcp.NewToolResultError("sql is required"), nil
	}

	rows, err := dbPool.QueryContext(ctx, query)
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

func dbListTables(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if dbPool == nil {
		return mcp.NewToolResultError("database not available"), nil
	}
	rows, err := dbPool.QueryContext(ctx, `
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
