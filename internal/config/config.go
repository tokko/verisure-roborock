package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// VacuumConfig holds per-device Roborock configuration.
type VacuumConfig struct {
	Host    string
	Token   string // 32-char hex device token
	Name    string // human-readable label for logs
	DID     string // Xiaomi cloud device ID; optional when host/token can match
	Backend string // "roborock", "xiaomi", or "local"
}

// Config holds all runtime configuration.
type Config struct {
	// Verisure
	VerisureEmail    string
	VerisurePassword string
	VerisureGIID     string // auto-discovered if empty
	VerisureBaseURL  string
	VerisureMFAPhone string // SMS 2FA phone number

	// Roborock account (for 'make fetch-tokens' — may differ from Verisure)
	RoborockEmail    string // falls back to XiaomiEmail then VerisureEmail
	RoborockPassword string // falls back to XiaomiPassword then VerisurePassword
	RoborockControl  string // "roborock"/"cloud" (Roborock app), "xiaomi", "mixed", or "local"
	RoborockAuthPath string // cached Roborock app auth for cloud control
	RoborockPython   string // Python executable for the Roborock app helper
	RoborockHelper   string // helper script path for Roborock app cloud control

	// Xiaomi cloud (alternative backend for Mi Home app users)
	XiaomiEmail    string
	XiaomiPassword string
	XiaomiCountry  string // EU=de, US=us, etc.
	XiaomiAuthPath string
	XiaomiAuth     XiaomiAuthConfig

	// Roborock vacuums (one or more)
	Vacuums []VacuumConfig

	// Timing
	PollInterval         time.Duration
	CleanCooldown        time.Duration
	CleanCooldownEnabled bool
	RoborockTimeout      time.Duration

	// Persistence
	StorePath string

	// Observability
	HTTPAddr  string
	LogLevel  string
	MFASecret string // optional bearer token protecting the /mfa-code endpoint
}

// Load reads configuration from .env (if present) then environment variables.
// Reports all missing required variables in one error.
func Load() (*Config, error) {
	LoadDotEnv(".env")

	var missing []string

	require := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}
	optional := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	duration := func(key string, def time.Duration) time.Duration {
		s := os.Getenv(key)
		if s == "" {
			return def
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			missing = append(missing, key+" (invalid duration: "+err.Error()+")")
			return def
		}
		return d
	}
	boolean := func(key string, def bool) bool {
		s := os.Getenv(key)
		if s == "" {
			return def
		}
		v, err := strconv.ParseBool(s)
		if err != nil {
			missing = append(missing, key+" (invalid bool: "+err.Error()+")")
			return def
		}
		return v
	}

	verisureEmail := require("VERISURE_EMAIL")
	verisurePassword := require("VERISURE_PASSWORD")

	cfg := &Config{
		VerisureEmail:    verisureEmail,
		VerisurePassword: verisurePassword,
		VerisureGIID:     optional("VERISURE_GIID", ""),
		VerisureBaseURL:  optional("VERISURE_BASE_URL", "https://e-api01.verisure.com/xbn/2"),
		VerisureMFAPhone: optional("VERISURE_MFA_PHONE", ""),

		// Roborock account — falls back through Xiaomi then Verisure credentials.
		RoborockEmail:    optional("ROBOROCK_EMAIL", optional("XIAOMI_EMAIL", verisureEmail)),
		RoborockPassword: optional("ROBOROCK_PASSWORD", optional("XIAOMI_PASSWORD", verisurePassword)),
		RoborockControl:  normalizeBackend(optional("ROBOROCK_CONTROL", "roborock")),
		RoborockAuthPath: optional("ROBOROCK_AUTH_PATH", "./roborock-auth.json"),
		RoborockPython:   optional("ROBOROCK_PYTHON", "python3"),
		RoborockHelper:   optional("ROBOROCK_HELPER", "./scripts/roborock_cloud.py"),

		XiaomiEmail:    optional("XIAOMI_EMAIL", optional("ROBOROCK_EMAIL", verisureEmail)),
		XiaomiPassword: optional("XIAOMI_PASSWORD", optional("ROBOROCK_PASSWORD", verisurePassword)),
		XiaomiCountry:  optional("XIAOMI_COUNTRY", "de"), // EU server
		XiaomiAuthPath: optional("XIAOMI_AUTH_PATH", "./xiaomi-auth.json"),
		XiaomiAuth: XiaomiAuthConfig{
			UserID:       optional("XIAOMI_USER_ID", ""),
			SSecurity:    optional("XIAOMI_SSECURITY", ""),
			ServiceToken: optional("XIAOMI_SERVICE_TOKEN", ""),
		},

		PollInterval:         duration("POLL_INTERVAL", 60*time.Second),
		CleanCooldown:        duration("CLEAN_COOLDOWN", 24*time.Hour),
		CleanCooldownEnabled: boolean("CLEAN_COOLDOWN_ENABLED", true),
		RoborockTimeout:      duration("ROBOROCK_TIMEOUT", 10*time.Second),
		StorePath:            optional("STORE_PATH", "./state.json"),
		HTTPAddr:             optional("HTTP_ADDR", ":8080"),
		LogLevel:             optional("LOG_LEVEL", "info"),
		MFASecret:            optional("MFA_SECRET", ""),
	}

	// Load vacuums: ROBOROCK_0_HOST, ROBOROCK_0_TOKEN, ROBOROCK_0_NAME, ...
	// Scan until a gap in the index.
	for i := 0; ; i++ {
		prefix := fmt.Sprintf("ROBOROCK_%d_", i)
		host := os.Getenv(prefix + "HOST")
		token := os.Getenv(prefix + "TOKEN")
		name := os.Getenv(prefix + "NAME")
		did := os.Getenv(prefix + "DID")
		backend := normalizeBackend(os.Getenv(prefix + "BACKEND"))
		if backend == "" {
			backend = cfg.RoborockControl
			if backend == "mixed" {
				backend = "roborock"
			}
		}
		if host == "" && token == "" && name == "" && did == "" && os.Getenv(prefix+"BACKEND") == "" {
			break
		}
		if backend == "local" && host == "" {
			missing = append(missing, prefix+"HOST")
		}
		if backend == "local" && token == "" {
			missing = append(missing, prefix+"TOKEN")
		}
		if backend == "xiaomi" && host == "" && token == "" && did == "" && name == "" {
			missing = append(missing, prefix+"DID or "+prefix+"HOST or "+prefix+"TOKEN")
		}
		if backend == "roborock" && host == "" && did == "" && name == "" {
			missing = append(missing, prefix+"NAME or "+prefix+"HOST or "+prefix+"DID")
		}
		if backend != "local" && backend != "roborock" && backend != "xiaomi" {
			missing = append(missing, prefix+"BACKEND (must be roborock, xiaomi, or local)")
		}
		if name == "" {
			name = fmt.Sprintf("vacuum-%d", i)
		}
		if host == "" {
			if did != "" {
				host = did
			} else if backend == "roborock" {
				host = name
			}
		}
		cfg.Vacuums = append(cfg.Vacuums, VacuumConfig{
			Host:    host,
			Token:   token,
			Name:    name,
			DID:     did,
			Backend: backend,
		})
	}

	if len(cfg.Vacuums) == 0 {
		missing = append(missing, "ROBOROCK_0_NAME or ROBOROCK_0_HOST or ROBOROCK_0_DID (at least one vacuum required)")
	}

	if cfg.RoborockControl != "roborock" && cfg.RoborockControl != "xiaomi" && cfg.RoborockControl != "mixed" && cfg.RoborockControl != "local" {
		missing = append(missing, "ROBOROCK_CONTROL (must be roborock, xiaomi, mixed, cloud, or local)")
	}

	// Guard against duplicate vacuum hosts (would cause a store key collision).
	seenHosts := make(map[string]bool, len(cfg.Vacuums))
	for _, v := range cfg.Vacuums {
		if seenHosts[v.Host] {
			missing = append(missing, fmt.Sprintf("duplicate ROBOROCK host %q — each vacuum must have a unique IP", v.Host))
		}
		seenHosts[v.Host] = true
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required configuration:\n  %s", strings.Join(missing, "\n  "))
	}

	return cfg, nil
}

type XiaomiAuthConfig struct {
	UserID       string
	SSecurity    string
	ServiceToken string
}

func (a XiaomiAuthConfig) Complete() bool {
	return a.UserID != "" && a.SSecurity != "" && a.ServiceToken != ""
}

func normalizeBackend(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "cloud" {
		return "roborock"
	}
	return v
}

// LoadDotEnv reads key=value pairs from path into the environment.
// Existing env vars are never overwritten.
// Lines starting with # and blank lines are ignored.
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip inline comments
		if i := strings.Index(line, " #"); i != -1 {
			line = strings.TrimSpace(line[:i])
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// Unquote simple quoted values
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
		}
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// VacuumsConfigured returns true if at least one vacuum has a token set.
func (c *Config) VacuumsConfigured() bool {
	for _, v := range c.Vacuums {
		if v.Token != "" {
			return true
		}
	}
	return false
}

// IndexedEnvName returns the env var name for a vacuum field, e.g. "ROBOROCK_0_TOKEN".
func IndexedEnvName(i int, field string) string {
	return fmt.Sprintf("ROBOROCK_%d_%s", i, field)
}

// FormatVacuumEnv formats discovered vacuum details as .env lines ready to paste.
func FormatVacuumEnv(i int, host, token, name string) string {
	return fmt.Sprintf("%s=%s\n%s=%s\n%s=%s",
		IndexedEnvName(i, "HOST"), host,
		IndexedEnvName(i, "TOKEN"), token,
		IndexedEnvName(i, "NAME"), name,
	)
}

// ParseIndex parses the vacuum index from an env var prefix like "ROBOROCK_2_".
func ParseIndex(prefix string) (int, error) {
	parts := strings.Split(strings.TrimSuffix(prefix, "_"), "_")
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid prefix: %s", prefix)
	}
	return strconv.Atoi(parts[len(parts)-1])
}
