// Package config loads runtime configuration from environment variables,
// optionally seeded from a .env file.
package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime settings for the whatsapp-mcp server.
type Config struct {
	// HTTPAddr is the address the MCP HTTP/SSE server listens on, e.g. ":8080".
	HTTPAddr string
	// PublicBaseURL is the externally reachable base URL for this server,
	// used to build the SSE endpoint advertised to MCP clients.
	PublicBaseURL string
	// WhatsAppDBPath is the sqlite file whatsmeow uses to persist session/device state.
	WhatsAppDBPath string
	// HistoryDBPath is the sqlite file used to persist chat/message history.
	HistoryDBPath string
	// LogLevel controls whatsmeow + app log verbosity: DEBUG, INFO, WARN, ERROR.
	LogLevel string
	// AuthToken, if set, is required as a Bearer token on every MCP request.
	AuthToken string
	// HistorySyncMaxAge, in days, bounds how much WhatsApp history backfill we keep.
	HistorySyncMaxAgeDays int
}

// Load reads runtime config from the process environment. If a .env file is
// present (path taken from DOTENV_PATH, default ".env" in the working
// directory) its values are loaded first; real environment variables always
// take precedence over anything in the file, so `FOO=bar go run .` still
// overrides .env as expected.
func Load() Config {
	loadDotEnv(getEnv("DOTENV_PATH", ".env"))

	return Config{
		HTTPAddr:              getEnv("HTTP_ADDR", ":8080"),
		PublicBaseURL:         getEnv("PUBLIC_BASE_URL", "http://localhost:8080"),
		WhatsAppDBPath:        getEnv("WHATSAPP_DB_PATH", "data/whatsapp.db"),
		HistoryDBPath:         getEnv("HISTORY_DB_PATH", "data/history.db"),
		LogLevel:              getEnv("LOG_LEVEL", "INFO"),
		AuthToken:             getEnv("MCP_AUTH_TOKEN", ""),
		HistorySyncMaxAgeDays: getEnvInt("HISTORY_SYNC_MAX_AGE_DAYS", 90),
	}
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// loadDotEnv parses a simple KEY=VALUE .env file and calls os.Setenv for any
// key not already present in the environment. Missing file is not an error
// (e.g. in Docker, config usually comes from real env vars instead).
// Supports blank lines, "# comment" lines, optional "export " prefixes, and
// single/double-quoted values.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}

		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		// Real environment variables win over .env, same as most tooling
		// (docker-compose, dotenv libraries, etc).
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
		}
	}
}
