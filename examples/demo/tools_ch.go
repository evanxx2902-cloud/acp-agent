package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var chPool *sql.DB

func registerCHTools(s *server.MCPServer) {
	chPool = connectCH()

	s.AddTool(mcp.NewTool("ch_query",
		mcp.WithDescription("Execute a SQL query against the ClickHouse database and return results."),
		mcp.WithString("sql", mcp.Description("SQL query to execute"), mcp.Required()),
	), chQuery)

	s.AddTool(mcp.NewTool("ch_list_tables",
		mcp.WithDescription("List all tables in the ClickHouse database."),
	), chListTables)
}

func connectCH() *sql.DB {
	password, err := os.ReadFile("/run/secure/service")
	if err != nil {
		fmt.Fprintf(os.Stderr, "MCP: failed to read CH password: %v (ch tools disabled)\n", err)
		return nil
	}
	pass := strings.TrimSpace(string(password))

	dsn := fmt.Sprintf("tcp://127.0.0.1:9000?username=default&database=audit&password=%s", pass)
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "MCP: failed to open CH: %v (ch tools disabled)\n", err)
		return nil
	}

	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "MCP: CH ping failed: %v (ch tools disabled)\n", err)
		db.Close()
		return nil
	}

	return db
}

func chQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if chPool == nil {
		return mcp.NewToolResultError("ClickHouse not available"), nil
	}
	query := req.GetString("sql", "")
	if query == "" {
		return mcp.NewToolResultError("sql is required"), nil
	}

	rows, err := chPool.QueryContext(ctx, query)
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

func chListTables(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if chPool == nil {
		return mcp.NewToolResultError("ClickHouse not available"), nil
	}
	rows, err := chPool.QueryContext(ctx, "SHOW TABLES")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list tables failed: %v", err)), nil
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		lines = append(lines, name)
	}
	if len(lines) == 0 {
		return mcp.NewToolResultText("(no tables)"), nil
	}
	return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
}
