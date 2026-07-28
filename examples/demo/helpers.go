package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/coder/acp-go-sdk"
)

func findAgentBinary() string {
	candidates := []string{"./agent-server", "../agent-server", "./acp", "../acp"}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	fmt.Println("Building agent...")
	cmd := exec.Command("go", "build", "-o", "/tmp/agent-server", ".")
	cmd.Dir = findProjectRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "Build failed: %v\n%s\n", err, string(out))
		os.Exit(1)
	}
	return "/tmp/agent-server"
}

func findProjectRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

func selfExe() string {
	p, _ := os.Executable()
	if p == "" {
		p, _ = filepath.Abs(os.Args[0])
	}
	return p
}

func mustCwd() string {
	wd, _ := os.Getwd()
	return wd
}

func dialAgent(addr string) (net.Conn, error) {
	switch {
	case strings.HasPrefix(addr, "tcp://"):
		return net.Dial("tcp", strings.TrimPrefix(addr, "tcp://"))
	case strings.HasPrefix(addr, "unix://"):
		return net.Dial("unix", strings.TrimPrefix(addr, "unix://"))
	default:
		return nil, fmt.Errorf("unsupported address scheme: %s (use tcp://host:port or unix:///path/to/sock)", addr)
	}
}

func die(method string, err error) {
	if re, ok := err.(*acp.RequestError); ok {
		b, _ := json.MarshalIndent(re, "", "  ")
		fmt.Fprintf(os.Stderr, "[%s] %s\n", method, string(b))
	} else {
		fmt.Fprintf(os.Stderr, "[%s] %v\n", method, err)
	}
	os.Exit(1)
}

func getIntArg(args map[string]any, key string, defaultVal int) int {
	v, ok := args[key]
	if !ok {
		return defaultVal
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return defaultVal
}
