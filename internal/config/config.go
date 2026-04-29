package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Server  Server
	Storage Storage
	Reaper  Reaper
	Policy  PolicyConfig
	Logs    Logs
}

type Server struct {
	Addr            string
	Token           string
	ShutdownTimeout time.Duration
	LogLevel        string
}

type Storage struct {
	DBPath string
}

type Reaper struct {
	Interval time.Duration
}

type Logs struct {
	BufferBytes int
}

type PolicyConfig struct {
	File     string
	Required bool
}

func Load() Config {
	_ = godotenv.Load()

	return Config{
		Server: Server{
			Addr:            envOr("KPD_ADDR", ":8080"),
			Token:           envOr("KPD_TOKEN", ""),
			ShutdownTimeout: envDuration("KPD_SHUTDOWN_TIMEOUT", 15*time.Second),
			LogLevel:        envOr("KPD_LOG_LEVEL", "info"),
		},
		Storage: Storage{
			DBPath: envOr("KPD_DB", "./kotakpasir.db"),
		},
		Reaper: Reaper{
			Interval: envDuration("KP_REAPER_INTERVAL", 30*time.Second),
		},
		Policy: PolicyConfig{
			File:     envOr("KP_POLICY_FILE", ""),
			Required: envBool("KP_REQUIRE_POLICY", false),
		},
		Logs: Logs{
			BufferBytes: envInt("KP_LOG_BUFFER_BYTES", 256*1024),
		},
	}
}

func envInt(key string, def int) int {
	return parseEnv(key, def, strconv.Atoi)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	return parseEnv(key, def, time.ParseDuration)
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func parseEnv[T any](key string, def T, parse func(string) (T, error)) T {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := parse(raw)
	if err != nil {
		return def
	}
	return v
}

func ParseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
