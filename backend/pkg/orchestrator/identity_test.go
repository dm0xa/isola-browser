package orchestrator

import (
	"reflect"
	"testing"
)

// TestIdentitySessionArgs pins the container env contract: a declared identity
// reaches the sandbox as TZ (container clock) plus the streamer's CDP inputs,
// and an undeclared one adds nothing, so the image's UTC / en-US are untouched.
func TestIdentitySessionArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		timezone string
		locale   string
		want     []string
	}{
		{name: "nothing declared yields no args", want: []string{}},
		{name: "blank values yield no args", timezone: "  ", locale: " ", want: []string{}},
		{
			name:     "both are passed",
			timezone: "Europe/Stockholm",
			locale:   "sv-SE",
			want: []string{
				"-e", "TZ=Europe/Stockholm",
				"-e", "ISOLA_SESSION_TZ=Europe/Stockholm",
				"-e", "ISOLA_SESSION_LOCALE=sv-SE",
			},
		},
		{
			name:     "timezone alone",
			timezone: "Asia/Tokyo",
			want:     []string{"-e", "TZ=Asia/Tokyo", "-e", "ISOLA_SESSION_TZ=Asia/Tokyo"},
		},
		{
			name:   "locale alone",
			locale: "en-GB",
			want:   []string{"-e", "ISOLA_SESSION_LOCALE=en-GB"},
		},
		{
			name:     "values are trimmed",
			timezone: " UTC ",
			locale:   " en-GB ",
			want: []string{
				"-e", "TZ=UTC",
				"-e", "ISOLA_SESSION_TZ=UTC",
				"-e", "ISOLA_SESSION_LOCALE=en-GB",
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := identitySessionArgs(tt.timezone, tt.locale)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("identitySessionArgs(%q, %q) = %v, want %v", tt.timezone, tt.locale, got, tt.want)
			}
		})
	}
}
