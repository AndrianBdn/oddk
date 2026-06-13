package util_test

import (
	"testing"

	"github.com/hypersequent/oddk/internal/util"
)

func TestParseRAMString(t *testing.T) {
	tests := []struct {
		input    string
		expected int
		hasError bool
	}{
		{"1024", 1048576, false},
		{"2", 2048, false},
		{"512M", 512, false},
		{"1024MB", 1024, false},
		{"2048MiB", 2048, false},
		{"1024 M", 1024, false},
		{"512 MB", 512, false},
		{"2048 MiB", 2048, false},
		{"", 0, true},
		{"abc", 0, true},
		{"123xyz", 0, true},
		{"-512", -524288, false},
		{"0", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := util.ParseRAMString(tt.input)
			if tt.hasError {
				if err == nil {
					t.Errorf("expected error for input %q, but got none", tt.input)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error for input %q: %v", tt.input, err)
				}
				if result != tt.expected {
					t.Errorf("for input %q, expected %d but got %d", tt.input, tt.expected, result)
				}
			}
		})
	}
}

func TestValidateSystemResources(t *testing.T) {
	totalCores, totalMemMB, err := util.GetSystemResourceLimits()
	if err != nil {
		t.Skipf("Cannot get system limits: %v", err)
	}

	tests := []struct {
		name     string
		cpuCores int
		ramMB    int
		hasError bool
	}{
		{"valid resources", 1, 512, false},
		{"zero CPU cores", 0, 512, true},
		{"negative CPU cores", -1, 512, true},
		{"too many CPU cores", totalCores + 1, 512, true},
		{"zero RAM", 1, 0, true},
		{"negative RAM", 1, -512, true},
		{"too much RAM", 1, totalMemMB + 1024, true},
		{"max CPU cores", totalCores, 512, false},
		{"max RAM (within limits)", 1, totalMemMB - 1024, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := util.ValidateSystemResources(tt.cpuCores, tt.ramMB)
			if tt.hasError {
				if err == nil {
					t.Errorf("expected error for CPU=%d, RAM=%d, but got none", tt.cpuCores, tt.ramMB)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error for CPU=%d, RAM=%d: %v", tt.cpuCores, tt.ramMB, err)
				}
			}
		})
	}
}

func TestValidateBasicResourceBounds(t *testing.T) {
	tests := []struct {
		name     string
		cpuCores int
		ramMB    int
		hasError bool
	}{
		{"valid resources", 4, 2048, false},
		{"minimum valid", 1, 128, false},
		{"maximum valid", 1024, 1048576, false},
		{"zero CPU cores", 0, 512, true},
		{"negative CPU cores", -1, 512, true},
		{"too many CPU cores", 1025, 512, true},
		{"too little RAM", 1, 127, true},
		{"too much RAM", 1, 1048577, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := util.ValidateBasicResourceBounds(tt.cpuCores, tt.ramMB)
			if tt.hasError {
				if err == nil {
					t.Errorf("expected error for CPU=%d, RAM=%d, but got none", tt.cpuCores, tt.ramMB)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error for CPU=%d, RAM=%d: %v", tt.cpuCores, tt.ramMB, err)
				}
			}
		})
	}
}
