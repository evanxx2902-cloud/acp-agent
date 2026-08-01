package main

import (
	"flag"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds server/transport/database/log configuration.
// LLM configuration is handled by LLMConfigProvider.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Transport TransportConfig `yaml:"transport"`
	Data      DataConfig      `yaml:"data"`
	Log       LogConfig       `yaml:"log"`
}

type ServerConfig struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

type TransportConfig struct {
	Type string      `yaml:"type"`
	TCP  TCPConfig   `yaml:"tcp"`
	Unix UnixConfig  `yaml:"unix"`
}

type TCPConfig struct {
	Listen string `yaml:"listen"`
}

type UnixConfig struct {
	Socket string `yaml:"socket"`
}

type DataConfig struct {
	Database DatabaseConfig `yaml:"database"`
}

type DatabaseConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}

func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{
			Name:    "agent-server",
			Version: "1.0.0",
		},
		Transport: TransportConfig{
			Type: "stdio",
		},
		Data: DataConfig{
			Database: DatabaseConfig{
				Driver: "sqlite3",
				DSN:    "file:./data/acp.db?cache=shared&_journal_mode=WAL&_fk=1",
			},
		},
		Log: LogConfig{
			Level: "info",
		},
	}
}

func LoadConfig() Config {
	cfg := DefaultConfig()

	configPath := flag.String("config", "", "Path to YAML or legacy JSON config file")
	llmConfigPath := flag.String("llm-config", "", "Path to LLM JSON config file")
	flag.Parse()

	if *configPath != "" {
		// If config ends with .json, treat as LLM config (legacy)
		if len(*configPath) > 5 && (*configPath)[len(*configPath)-5:] == ".json" {
			os.Setenv("ACP_LLM_CONFIG_PATH", *configPath)
		} else {
			data, err := os.ReadFile(*configPath)
			if err == nil {
				yaml.Unmarshal(data, &cfg)
			}
		}
	}
	if *llmConfigPath != "" {
		os.Setenv("ACP_LLM_CONFIG_PATH", *llmConfigPath)
	}

	// Env var overrides
	if v := os.Getenv("ACP_LISTEN"); v != "" {
		switch {
		case strings.HasPrefix(v, "tcp://"):
			cfg.Transport.Type = "tcp"
			cfg.Transport.TCP.Listen = strings.TrimPrefix(v, "tcp://")
		case strings.HasPrefix(v, "unix://"):
			cfg.Transport.Type = "unix"
			cfg.Transport.Unix.Socket = strings.TrimPrefix(v, "unix://")
		default:
			cfg.Transport.Type = "stdio"
		}
	}
	if v := os.Getenv("ACP_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("ACP_DB_DSN"); v != "" {
		cfg.Data.Database.DSN = v
	}

	return cfg
}

// GetListenAddr returns the effective listen address or socket path.
func (c Config) GetListenAddr() string {
	switch c.Transport.Type {
	case "tcp":
		if c.Transport.TCP.Listen != "" {
			return "tcp://" + c.Transport.TCP.Listen
		}
	case "unix":
		if c.Transport.Unix.Socket != "" {
			return "unix://" + c.Transport.Unix.Socket
		}
	}
	return "stdio"
}
