package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeTimezone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "empty stays empty", in: "", want: ""},
		{name: "whitespace only stays empty", in: "   ", want: ""},
		{name: "IANA zone is kept", in: "Europe/Stockholm", want: "Europe/Stockholm"},
		{name: "surrounding whitespace is trimmed", in: "  Asia/Tokyo  ", want: "Asia/Tokyo"},
		{name: "lowercase zone resolves to canonical case", in: "europe/stockholm", want: "Europe/Stockholm"},
		{name: "UTC is accepted", in: "UTC", want: "UTC"},
		{name: "unknown zone is rejected", in: "Europe/Atlantis", wantErr: true},
		{name: "empty segment is rejected", in: "Europe/", wantErr: true},
		{name: "host-local zone is rejected", in: "Local", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeTimezone(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeTimezone(%q) = %q, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeTimezone(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeTimezone(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeLocale(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "empty stays empty", in: "", want: ""},
		{name: "whitespace only stays empty", in: "  ", want: ""},
		{name: "primary language only", in: "sv", want: "sv"},
		{name: "language and region are canonicalized", in: "sv-se", want: "sv-SE"},
		{name: "language is lowercased", in: "SV-SE", want: "sv-SE"},
		{name: "script is titlecased", in: "zh-hant-tw", want: "zh-Hant-TW"},
		{name: "numeric region is kept", in: "es-419", want: "es-419"},
		{name: "surrounding whitespace is trimmed", in: " en-GB ", want: "en-GB"},
		{name: "one-letter language is rejected", in: "e-US", wantErr: true},
		{name: "underscore separator is rejected", in: "sv_SE", wantErr: true},
		{name: "trailing separator is rejected", in: "sv-", wantErr: true},
		{name: "empty subtag is rejected", in: "sv--SE", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeLocale(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeLocale(%q) = %q, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeLocale(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeLocale(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestLoadFromSessionIdentity pins the deployment-default wiring: YAML values
// are parsed and normalized, env overrides them, and an unusable value fails
// startup instead of poisoning every session.
func TestLoadFromSessionIdentity(t *testing.T) {
	// Not parallel: subtests use t.Setenv.
	writeCfg := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("defaults stay empty", func(t *testing.T) {
		t.Setenv("ISOLA_SESSION_TIMEZONE", "")
		t.Setenv("ISOLA_SESSION_LOCALE", "")
		cfg, err := LoadFrom(writeCfg(t, "server:\n  port: \"8080\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SessionTimezone != "" || cfg.SessionLocale != "" {
			t.Fatalf("defaults = %q/%q, want empty", cfg.SessionTimezone, cfg.SessionLocale)
		}
	})

	t.Run("yaml values are normalized", func(t *testing.T) {
		t.Setenv("ISOLA_SESSION_TIMEZONE", "")
		t.Setenv("ISOLA_SESSION_LOCALE", "")
		cfg, err := LoadFrom(writeCfg(t, "session:\n  timezone: \"europe/stockholm\"\n  locale: \"sv-se\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SessionTimezone != "Europe/Stockholm" || cfg.SessionLocale != "sv-SE" {
			t.Fatalf("yaml identity = %q/%q", cfg.SessionTimezone, cfg.SessionLocale)
		}
	})

	t.Run("env overrides yaml", func(t *testing.T) {
		t.Setenv("ISOLA_SESSION_TIMEZONE", "Asia/Tokyo")
		t.Setenv("ISOLA_SESSION_LOCALE", "ja-JP")
		cfg, err := LoadFrom(writeCfg(t, "session:\n  timezone: \"Europe/Stockholm\"\n  locale: \"sv-SE\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SessionTimezone != "Asia/Tokyo" || cfg.SessionLocale != "ja-JP" {
			t.Fatalf("env identity = %q/%q", cfg.SessionTimezone, cfg.SessionLocale)
		}
	})

	t.Run("unknown zone fails startup", func(t *testing.T) {
		t.Setenv("ISOLA_SESSION_TIMEZONE", "Europe/Atlantis")
		t.Setenv("ISOLA_SESSION_LOCALE", "")
		if _, err := LoadFrom(writeCfg(t, "server:\n  port: \"8080\"\n")); err == nil {
			t.Fatal("LoadFrom accepted an unknown timezone")
		}
	})

	t.Run("malformed locale fails startup", func(t *testing.T) {
		t.Setenv("ISOLA_SESSION_TIMEZONE", "")
		t.Setenv("ISOLA_SESSION_LOCALE", "sv_SE")
		if _, err := LoadFrom(writeCfg(t, "server:\n  port: \"8080\"\n")); err == nil {
			t.Fatal("LoadFrom accepted a malformed locale")
		}
	})
}
