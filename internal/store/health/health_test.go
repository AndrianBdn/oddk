package health_test

import (
	"testing"
	"time"

	"github.com/hypersequent/oddk/internal/store/health"
)

func TestHealthRecord_GetTimestamp(t *testing.T) {
	testTime := time.Now().Unix()
	record := &health.HealthRecord{
		TsUnix: testTime,
	}

	result := record.GetTimestamp()
	if result.Unix() != testTime {
		t.Errorf("GetTimestamp() returned %d, want %d", result.Unix(), testTime)
	}
}

func TestHealthRecord_GetHealthyInstancesList(t *testing.T) {
	tests := []struct {
		name             string
		healthyInstances string
		want             []string
	}{
		{
			name:             "empty string",
			healthyInstances: "",
			want:             []string{},
		},
		{
			name:             "single instance",
			healthyInstances: "db1",
			want:             []string{"db1"},
		},
		{
			name:             "multiple instances",
			healthyInstances: "db1,db2,db3",
			want:             []string{"db1", "db2", "db3"},
		},
		{
			name:             "instances with spaces",
			healthyInstances: "db1, db2 , db3",
			want:             []string{"db1", "db2", "db3"},
		},
		{
			name:             "empty elements filtered out",
			healthyInstances: "db1,,db2,",
			want:             []string{"db1", "db2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := &health.HealthRecord{
				HealthyInstances: tt.healthyInstances,
			}

			result := record.GetHealthyInstancesList()

			if len(result) != len(tt.want) {
				t.Errorf("GetHealthyInstancesList() returned %d items, want %d", len(result), len(tt.want))
				return
			}

			for i, want := range tt.want {
				if result[i] != want {
					t.Errorf("GetHealthyInstancesList()[%d] = %q, want %q", i, result[i], want)
				}
			}
		})
	}
}

func TestHealthRecord_GetBrokenInstancesList(t *testing.T) {
	tests := []struct {
		name            string
		brokenInstances string
		want            []string
	}{
		{
			name:            "empty string",
			brokenInstances: "",
			want:            []string{},
		},
		{
			name:            "single instance",
			brokenInstances: "broken1",
			want:            []string{"broken1"},
		},
		{
			name:            "multiple instances",
			brokenInstances: "broken1,broken2",
			want:            []string{"broken1", "broken2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := &health.HealthRecord{
				BrokenInstances: tt.brokenInstances,
			}

			result := record.GetBrokenInstancesList()

			if len(result) != len(tt.want) {
				t.Errorf("GetBrokenInstancesList() returned %d items, want %d", len(result), len(tt.want))
				return
			}

			for i, want := range tt.want {
				if result[i] != want {
					t.Errorf("GetBrokenInstancesList()[%d] = %q, want %q", i, result[i], want)
				}
			}
		})
	}
}
