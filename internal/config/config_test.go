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
