package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	t.Run("valid key=value", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		content := "FOO_TEST_KEY=bar\nBAZ_TEST_KEY=qux\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Unsetenv("FOO_TEST_KEY")
		os.Unsetenv("BAZ_TEST_KEY")
		t.Cleanup(func() {
			os.Unsetenv("FOO_TEST_KEY")
			os.Unsetenv("BAZ_TEST_KEY")
		})

		LoadDotEnv(path)
		if got := os.Getenv("FOO_TEST_KEY"); got != "bar" {
			t.Errorf("FOO_TEST_KEY = %q, want bar", got)
		}
		if got := os.Getenv("BAZ_TEST_KEY"); got != "qux" {
			t.Errorf("BAZ_TEST_KEY = %q, want qux", got)
		}
	})

	t.Run("comments and blank lines ignored", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		content := "# leading comment\n\nCOMMENT_TEST=hello\n# another\n\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Unsetenv("COMMENT_TEST")
		t.Cleanup(func() { os.Unsetenv("COMMENT_TEST") })

		LoadDotEnv(path)
		if got := os.Getenv("COMMENT_TEST"); got != "hello" {
			t.Errorf("COMMENT_TEST = %q, want hello", got)
		}
	})

	t.Run("inline comment stripped", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		content := "INLINE_TEST=value # trailing comment\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Unsetenv("INLINE_TEST")
		t.Cleanup(func() { os.Unsetenv("INLINE_TEST") })

		LoadDotEnv(path)
		if got := os.Getenv("INLINE_TEST"); got != "value" {
			t.Errorf("INLINE_TEST = %q, want value", got)
		}
	})

	t.Run("quoted values unquoted", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		content := "QUOTED_TEST=\"my value\"\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Unsetenv("QUOTED_TEST")
		t.Cleanup(func() { os.Unsetenv("QUOTED_TEST") })

		LoadDotEnv(path)
		if got := os.Getenv("QUOTED_TEST"); got != "my value" {
			t.Errorf("QUOTED_TEST = %q, want 'my value'", got)
		}
	})

	t.Run("does not overwrite existing", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		content := "EXISTING_TEST=from_file\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Setenv("EXISTING_TEST", "preset")
		t.Cleanup(func() { os.Unsetenv("EXISTING_TEST") })

		LoadDotEnv(path)
		if got := os.Getenv("EXISTING_TEST"); got != "preset" {
			t.Errorf("EXISTING_TEST = %q, want preset (must not overwrite)", got)
		}
	})

	t.Run("missing file silent", func(t *testing.T) {
		// Should not panic or error.
		LoadDotEnv(filepath.Join(t.TempDir(), "does-not-exist"))
	})
}

func TestFormatVacuumEnv(t *testing.T) {
	got := FormatVacuumEnv(0, "10.0.0.1", "abc123", "kitchen")
	want := "ROBOROCK_0_HOST=10.0.0.1\nROBOROCK_0_TOKEN=abc123\nROBOROCK_0_NAME=kitchen"
	if got != want {
		t.Errorf("FormatVacuumEnv(0,...) =\n%q\nwant\n%q", got, want)
	}

	got2 := FormatVacuumEnv(2, "h", "t", "n")
	if !strings.Contains(got2, "ROBOROCK_2_HOST=h") ||
		!strings.Contains(got2, "ROBOROCK_2_TOKEN=t") ||
		!strings.Contains(got2, "ROBOROCK_2_NAME=n") {
		t.Errorf("FormatVacuumEnv(2,...) missing expected fields: %q", got2)
	}
}

func TestIndexedEnvName(t *testing.T) {
	if got := IndexedEnvName(0, "HOST"); got != "ROBOROCK_0_HOST" {
		t.Errorf("got %q, want ROBOROCK_0_HOST", got)
	}
	if got := IndexedEnvName(3, "TOKEN"); got != "ROBOROCK_3_TOKEN" {
		t.Errorf("got %q, want ROBOROCK_3_TOKEN", got)
	}
}

func TestParseIndex(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"ROBOROCK_0_", 0, false},
		{"ROBOROCK_5_", 5, false},
		{"BAD", 0, true},
	}
	for _, c := range cases {
		got, err := ParseIndex(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseIndex(%q): expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseIndex(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseIndex(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestLoadRoborockControl(t *testing.T) {
	t.Run("loads clean cooldown feature flag", func(t *testing.T) {
		withEnv(t, map[string]string{
			"VERISURE_EMAIL":         "v@example.com",
			"VERISURE_PASSWORD":      "secret",
			"ROBOROCK_CONTROL":       "roborock",
			"ROBOROCK_0_NAME":        "upstairs",
			"CLEAN_COOLDOWN_ENABLED": "false",
			"CLEAN_COOLDOWN":         "24h",
			"ROBOROCK_0_HOST":        "",
			"ROBOROCK_0_TOKEN":       "",
			"ROBOROCK_TIMEOUT":       "",
			"POLL_INTERVAL":          "",
			"STORE_PATH":             "",
			"HTTP_ADDR":              "",
			"LOG_LEVEL":              "",
			"VERISURE_BASE_URL":      "",
			"VERISURE_GIID":          "",
			"VERISURE_MFA_PHONE":     "",
			"ROBOROCK_AUTH_PATH":     "",
			"XIAOMI_AUTH_PATH":       "",
			"XIAOMI_USER_ID":         "",
			"XIAOMI_SSECURITY":       "",
			"XIAOMI_SERVICE_TOKEN":   "",
			"ROBOROCK_0_DID":         "",
			"ROBOROCK_0_BACKEND":     "",
			"ROBOROCK_1_HOST":        "",
			"ROBOROCK_1_TOKEN":       "",
			"ROBOROCK_1_NAME":        "",
			"ROBOROCK_1_DID":         "",
			"ROBOROCK_1_BACKEND":     "",
		})

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.CleanCooldownEnabled {
			t.Fatal("CleanCooldownEnabled = true, want false")
		}
	})

	t.Run("roborock cloud mode accepts named app devices without local token", func(t *testing.T) {
		withEnv(t, map[string]string{
			"VERISURE_EMAIL":       "v@example.com",
			"VERISURE_PASSWORD":    "secret",
			"ROBOROCK_CONTROL":     "roborock",
			"ROBOROCK_0_NAME":      "upstairs",
			"ROBOROCK_1_NAME":      "downstairs",
			"ROBOROCK_1_HOST":      "192.0.2.20",
			"ROBOROCK_EMAIL":       "r@example.com",
			"ROBOROCK_PASSWORD":    "rr-secret",
			"XIAOMI_COUNTRY":       "de",
			"XIAOMI_AUTH_PATH":     "/data/xiaomi-auth.json",
			"ROBOROCK_AUTH_PATH":   "/data/roborock-auth.json",
			"XIAOMI_USER_ID":       "42",
			"XIAOMI_SSECURITY":     "sec",
			"XIAOMI_SERVICE_TOKEN": "token",
			"ROBOROCK_0_HOST":      "",
			"ROBOROCK_0_TOKEN":     "",
			"ROBOROCK_TIMEOUT":     "",
			"POLL_INTERVAL":        "",
			"CLEAN_COOLDOWN":       "",
			"STORE_PATH":           "",
			"HTTP_ADDR":            "",
			"LOG_LEVEL":            "",
			"VERISURE_BASE_URL":    "",
			"VERISURE_GIID":        "",
			"VERISURE_MFA_PHONE":   "",
		})

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.RoborockControl != "roborock" {
			t.Fatalf("RoborockControl = %q, want roborock", cfg.RoborockControl)
		}
		if got := cfg.Vacuums[0].Backend; got != "roborock" {
			t.Fatalf("Backend = %q, want roborock", got)
		}
		if got := cfg.Vacuums[0].Host; got != "upstairs" {
			t.Fatalf("roborock store key Host = %q, want name fallback", got)
		}
		if got := cfg.XiaomiEmail; got != "r@example.com" {
			t.Fatalf("XiaomiEmail = %q, want Roborock fallback", got)
		}
		if got := cfg.RoborockAuthPath; got != "/data/roborock-auth.json" {
			t.Fatalf("RoborockAuthPath = %q", got)
		}
		if cfg.XiaomiAuthPath != "/data/xiaomi-auth.json" || !cfg.XiaomiAuth.Complete() {
			t.Fatalf("xiaomi auth config not loaded: %+v path=%q", cfg.XiaomiAuth, cfg.XiaomiAuthPath)
		}
	})

	t.Run("per-vacuum backend can keep Xiaomi-only Gizmo separate", func(t *testing.T) {
		withEnv(t, map[string]string{
			"VERISURE_EMAIL":     "v@example.com",
			"VERISURE_PASSWORD":  "secret",
			"ROBOROCK_CONTROL":   "roborock",
			"ROBOROCK_0_NAME":    "upstairs",
			"ROBOROCK_1_NAME":    "gizmo",
			"ROBOROCK_1_BACKEND": "xiaomi",
			"ROBOROCK_1_DID":     "118097498",
		})

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Vacuums[0].Backend != "roborock" || cfg.Vacuums[1].Backend != "xiaomi" {
			t.Fatalf("backends = %q/%q, want roborock/xiaomi", cfg.Vacuums[0].Backend, cfg.Vacuums[1].Backend)
		}
	})

	t.Run("local mode still requires host and token", func(t *testing.T) {
		withEnv(t, map[string]string{
			"VERISURE_EMAIL":    "v@example.com",
			"VERISURE_PASSWORD": "secret",
			"ROBOROCK_CONTROL":  "local",
			"ROBOROCK_0_DID":    "123456789",
		})

		_, err := Load()
		if err == nil {
			t.Fatal("Load succeeded, want missing host/token error")
		}
		if !strings.Contains(err.Error(), "ROBOROCK_0_HOST") ||
			!strings.Contains(err.Error(), "ROBOROCK_0_TOKEN") {
			t.Fatalf("error = %q, want missing host and token", err)
		}
	})
}

func withEnv(t *testing.T, vals map[string]string) {
	t.Helper()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Fatal(err)
		}
	})
	keys := []string{
		"VERISURE_EMAIL",
		"VERISURE_PASSWORD",
		"VERISURE_GIID",
		"VERISURE_BASE_URL",
		"VERISURE_MFA_PHONE",
		"ROBOROCK_EMAIL",
		"ROBOROCK_PASSWORD",
		"ROBOROCK_CONTROL",
		"ROBOROCK_AUTH_PATH",
		"ROBOROCK_PYTHON",
		"ROBOROCK_HELPER",
		"XIAOMI_EMAIL",
		"XIAOMI_PASSWORD",
		"XIAOMI_COUNTRY",
		"XIAOMI_AUTH_PATH",
		"XIAOMI_USER_ID",
		"XIAOMI_SSECURITY",
		"XIAOMI_SERVICE_TOKEN",
		"ROBOROCK_0_HOST",
		"ROBOROCK_0_TOKEN",
		"ROBOROCK_0_NAME",
		"ROBOROCK_0_DID",
		"ROBOROCK_0_BACKEND",
		"ROBOROCK_1_HOST",
		"ROBOROCK_1_TOKEN",
		"ROBOROCK_1_NAME",
		"ROBOROCK_1_DID",
		"ROBOROCK_1_BACKEND",
		"ROBOROCK_TIMEOUT",
		"POLL_INTERVAL",
		"CLEAN_COOLDOWN",
		"CLEAN_COOLDOWN_ENABLED",
		"STORE_PATH",
		"HTTP_ADDR",
		"LOG_LEVEL",
	}
	for _, key := range keys {
		old, ok := os.LookupEnv(key)
		if v, exists := vals[key]; exists {
			if v == "" {
				os.Unsetenv(key)
			} else {
				os.Setenv(key, v)
			}
		} else {
			os.Unsetenv(key)
		}
		t.Cleanup(func() {
			if ok {
				os.Setenv(key, old)
			} else {
				os.Unsetenv(key)
			}
		})
	}
}
