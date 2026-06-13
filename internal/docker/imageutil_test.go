package docker_test

import (
	"testing"

	"github.com/hypersequent/oddk/internal/docker"
)

func TestDetectPGVersionFromImage(t *testing.T) {
	tests := []struct {
		image  string
		want   string
		wantOK bool
	}{
		{"postgres:17", "17", true},
		{"postgres:17.2", "17", true},
		{"postgres:16.1-alpine", "16", true},
		{"pgvector/pgvector:pg18-trixie", "18", true},
		{"pgvector/pgvector:0.8.2-pg18", "18", true},
		{"postgis/postgis:18-3.6", "18", true},
		{"postgis/postgis:15-3.3", "15", true},
		{"myimage:latest", "", false},
		{"myimage:v1.0.0", "", false},
		{"postgres:", "", false},
	}

	for _, tt := range tests {
		got, ok := docker.DetectPGVersionFromImage(tt.image)
		if ok != tt.wantOK || got != tt.want {
			t.Errorf("DetectPGVersionFromImage(%q) = (%q, %v), want (%q, %v)",
				tt.image, got, ok, tt.want, tt.wantOK)
		}
	}
}
