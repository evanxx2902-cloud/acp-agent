package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerFSTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("read_file",
		mcp.WithDescription("Read the contents of a file. Returns the file content as text."),
		mcp.WithString("path", mcp.Description("Absolute path to the file to read"), mcp.Required()),
		mcp.WithNumber("offset", mcp.Description("Line number to start reading from (1-based)")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of lines to read")),
	), fsReadFile)

	s.AddTool(mcp.NewTool("write_file",
		mcp.WithDescription("Write or overwrite a file with the given content."),
		mcp.WithString("path", mcp.Description("Absolute path to the file"), mcp.Required()),
		mcp.WithString("content", mcp.Description("Content to write"), mcp.Required()),
	), fsWriteFile)

	s.AddTool(mcp.NewTool("list_directory",
		mcp.WithDescription("List the contents of a directory. Returns file names, sizes, types, and permissions."),
		mcp.WithString("path", mcp.Description("Absolute path to the directory"), mcp.Required()),
	), fsListDir)

	s.AddTool(mcp.NewTool("make_directory",
		mcp.WithDescription("Create a new directory, including any necessary parent directories."),
		mcp.WithString("path", mcp.Description("Absolute path to create"), mcp.Required()),
	), fsMkdir)

	s.AddTool(mcp.NewTool("move_file",
		mcp.WithDescription("Move or rename a file or directory."),
		mcp.WithString("source", mcp.Description("Source path"), mcp.Required()),
		mcp.WithString("destination", mcp.Description("Destination path"), mcp.Required()),
	), fsMove)

	s.AddTool(mcp.NewTool("delete_file",
		mcp.WithDescription("Delete a file or directory. Directories are removed recursively."),
		mcp.WithString("path", mcp.Description("Path to delete"), mcp.Required()),
	), fsDelete)

	s.AddTool(mcp.NewTool("get_file_info",
		mcp.WithDescription("Get metadata about a file or directory: size, permissions, modification time."),
		mcp.WithString("path", mcp.Description("Absolute path"), mcp.Required()),
	), fsStat)

	s.AddTool(mcp.NewTool("search_files",
		mcp.WithDescription("Search for files matching a glob pattern in a directory."),
		mcp.WithString("directory", mcp.Description("Directory to search in"), mcp.Required()),
		mcp.WithString("pattern", mcp.Description("Glob pattern (e.g., '*.go', '**/*.md')"), mcp.Required()),
	), fsSearch)
}

func fsReadFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	data, err := os.ReadFile(path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read %s: %v", path, err)), nil
	}
	lines := strings.Split(string(data), "\n")
	args := req.GetArguments()
	offset := getIntArg(args, "offset", 1) - 1
	if offset < 0 {
		offset = 0
	}
	if offset >= len(lines) {
		return mcp.NewToolResultText(""), nil
	}
	lines = lines[offset:]
	limit := getIntArg(args, "limit", 0)
	if limit > 0 && limit < len(lines) {
		lines = lines[:limit]
	}
	return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
}

func fsWriteFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	content := req.GetString("content", "")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("mkdir: %v", err)), nil
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("write %s: %v", path, err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Wrote %d bytes to %s", len(content), path)), nil
}

func fsListDir(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	entries, err := os.ReadDir(path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read dir %s: %v", path, err)), nil
	}
	var lines []string
	for _, e := range entries {
		info, _ := e.Info()
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		if info != nil {
			lines = append(lines, fmt.Sprintf("%s %8d %s", info.Mode(), info.Size(), name))
		} else {
			lines = append(lines, name)
		}
	}
	if len(lines) == 0 {
		return mcp.NewToolResultText("(empty)"), nil
	}
	return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
}

func fsMkdir(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	if err := os.MkdirAll(path, 0755); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("mkdir %s: %v", path, err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Created %s", path)), nil
}

func fsMove(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	src := req.GetString("source", "")
	dst := req.GetString("destination", "")
	if err := os.Rename(src, dst); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("move: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("%s -> %s", src, dst)), nil
}

func fsDelete(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	info, err := os.Stat(path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("stat %s: %v", path, err)), nil
	}
	if info.IsDir() {
		err = os.RemoveAll(path)
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("delete %s: %v", path, err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Deleted %s", path)), nil
}

func fsStat(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	info, err := os.Stat(path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("stat %s: %v", path, err)), nil
	}
	result := fmt.Sprintf("Path: %s\nSize: %d\nMode: %s\nIsDir: %v\nModTime: %s",
		path, info.Size(), info.Mode(), info.IsDir(),
		info.ModTime().Format("2006-01-02 15:04:05"))
	return mcp.NewToolResultText(result), nil
}

func fsSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	dir := req.GetString("directory", "")
	pattern := req.GetString("pattern", "")
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("glob: %v", err)), nil
	}
	if len(matches) == 0 {
		return mcp.NewToolResultText("(no matches)"), nil
	}
	return mcp.NewToolResultText(strings.Join(matches, "\n")), nil
}
