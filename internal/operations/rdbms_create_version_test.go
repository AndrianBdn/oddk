package operations

import (
	"errors"
	"testing"

	"github.com/andrianbdn/oddk/internal/operr"
)

// The recorded major decides the data-volume mount, so a wrong guess loses the
// data on the next recreate: the image's PG_MAJOR wins, then its tag, and an
// image that says neither is refused rather than given the default.
func TestResolveCreateVersion(t *testing.T) {
	pgMajors := map[string]int{
		"postgres:17":                   17,
		"docker.io/library/postgres:17": 17,
		"pgvector/pgvector:pg17-trixie": 17,
		"myco/pg:latest":                17,
		"postgres:19":                   19,
		"myco/pg:1.2.0":                 18,
	}
	imageMajor := func(image string) (int, bool) {
		m, ok := pgMajors[image]
		return m, ok
	}
	for _, tc := range []struct {
		version, image string
		want           string
		wantInvalid    bool
	}{
		{version: "", image: "", want: DefaultPostgresVersion},
		{version: "16", image: "", want: "16"},
		{version: "18 ", image: "postgres:18", wantInvalid: true}, // would select the PG <= 17 mount
		{version: " 18", image: "", wantInvalid: true},
		{version: "abc", image: "", wantInvalid: true},
		{version: "", image: "docker.io/library/postgres:17", want: "17"}, // tag detection misses this
		{version: "", image: "myco/pg:latest", want: "17"},                // no major in the tag
		{version: "", image: "postgres:19", want: "19"},
		{version: "", image: "pgvector/pgvector:pg16-trixie", want: "16"}, // not pulled: tag
		{version: "", image: "example/custom:latest", wantInvalid: true},  // says nothing
		{version: "18", image: "myco/pg:latest", wantInvalid: true},       // contradicts PG_MAJOR
		{version: "17", image: "myco/pg:latest", want: "17"},
		{version: "17.2", image: "postgres:17", want: "17.2"},    // same major
		{version: "", image: "myco/pg:1.2.0", wantInvalid: true}, // tag says 1, PG_MAJOR 18: switch/update read the tag
		{version: "18", image: "myco/pg:1.2.0", wantInvalid: true},
		{version: "16", image: "pgvector/pgvector:pg17-trixie", wantInvalid: true}, // contradicts PG_MAJOR
		{version: "18", image: "pgvector/pgvector:pg16-trixie", wantInvalid: true}, // not pulled: contradicts the tag
		{version: "18", image: "example/custom:latest", want: "18"},                // image says nothing: trust --version
		{version: "", image: "noenv/pg:1.2.0", wantInvalid: true},                  // no PG_MAJOR; "1" is a release, not a major
		{version: "", image: "noenv/pg:2026.11", wantInvalid: true},                // likewise
		{version: "", image: "noenv/pg:9.6", want: "9"},                            // lowest believed tag major
		{version: "", image: "noenv/pg:99-x", want: "99"},                          // highest
		{version: "", image: "noenv/pg:8.4", wantInvalid: true},                    // below the bound
		{version: "", image: "noenv/pg:100-x", wantInvalid: true},                  // above it
		{version: "18", image: "noenv/pg:1.2.0", wantInvalid: true},                // switch/update would read "1" from the tag
	} {
		got, err := resolveCreateVersion(tc.version, tc.image, imageMajor)
		if tc.wantInvalid {
			if !errors.Is(err, operr.ErrInvalid) {
				t.Errorf("resolveCreateVersion(%q, %q) = (%q, %v), want an ErrInvalid refusal", tc.version, tc.image, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("resolveCreateVersion(%q, %q) = (%q, %v), want %q", tc.version, tc.image, got, err, tc.want)
		}
	}
}

// Every path that takes --version shares one shape check, because the stored
// string is parsed untrimmed where it picks the data mount.
func TestValidateVersionShape(t *testing.T) {
	for _, ok := range []string{"", "18", "17.2", "9"} {
		if err := validateVersionShape(ok); err != nil {
			t.Errorf("validateVersionShape(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"18 ", " 18", "18\n", "v18", "18.1.2", "18.", "abc"} {
		if err := validateVersionShape(bad); !errors.Is(err, operr.ErrInvalid) {
			t.Errorf("validateVersionShape(%q) = %v, want an ErrInvalid refusal", bad, err)
		}
	}
}
