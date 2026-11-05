package operations

import (
	"errors"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store/parameters"
)

// The 2026-11-05 ap1 cut: an instance created on a 16 GB host kept its 8 GB
// size after the host shrank to 8 GB, so its own snapshot no longer fit an
// identical machine.
func TestFitInstanceSizes(t *testing.T) {
	tests := []struct {
		name               string
		cpu, ram           int
		wantCPU, wantRAM   int
		wantResized        bool
		wantNote, wantFail string
	}{
		{
			name: "ram over the host", cpu: 2, ram: 8192, wantCPU: 2, wantRAM: 7762, wantResized: true,
			wantNote: "qasphere: 8192 → 7762 MB to fit this host",
		},
		{
			name: "cpu over the host", cpu: 4, ram: 2048, wantCPU: 2, wantRAM: 2048, wantResized: true,
			wantNote: "qasphere: 4 → 2 CPU to fit this host",
		},
		{
			name: "both over the host", cpu: 8, ram: 16384, wantCPU: 2, wantRAM: 7762, wantResized: true,
			wantNote: "qasphere: 16384 → 7762 MB, 8 → 2 CPU to fit this host",
		},
		{name: "fits exactly", cpu: 2, ram: 7762, wantCPU: 2, wantRAM: 7762},
		{name: "fits", cpu: 1, ram: 512, wantCPU: 1, wantRAM: 512},
		{name: "no ram recorded", cpu: 1, ram: 0, wantFail: "0 MB RAM"},
		{name: "no cpu recorded", cpu: 0, ram: 512, wantFail: "0 CPU"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			meta := &InstanceMeta{Name: "qasphere", CPUCores: tc.cpu, RAMMB: tc.ram}
			resized, err := fitInstanceSizes([]*InstanceMeta{meta}, 2, 7762)
			if tc.wantFail != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantFail) || !errors.Is(err, operr.ErrInvalid) {
					t.Fatalf("err = %v, want an invalid-input refusal mentioning %q", err, tc.wantFail)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if meta.CPUCores != tc.wantCPU || meta.RAMMB != tc.wantRAM {
				t.Errorf("fitted to %d CPU / %d MB, want %d / %d", meta.CPUCores, meta.RAMMB, tc.wantCPU, tc.wantRAM)
			}
			if !tc.wantResized {
				if len(resized) != 0 {
					t.Errorf("resized = %v, want none", resized)
				}
				return
			}
			if len(resized) != 1 || resized[0].String() != tc.wantNote {
				t.Errorf("resized = %v, want one entry %q", resized, tc.wantNote)
			}
			r := resized[0]
			if r.FromCPUCores != tc.cpu || r.FromRAMMB != tc.ram || r.CPUCores != tc.wantCPU || r.RAMMB != tc.wantRAM {
				t.Errorf("resized = %+v, does not record the recorded and fitted sizes", r)
			}
		})
	}
}

func TestCheckParameterGroupFits(t *testing.T) {
	group := func(sharedBuffers string) *parameters.ParameterGroup {
		return &parameters.ParameterGroup{Parameters: []parameters.Parameter{{
			Name: "shared_buffers", Type: parameters.ParameterTypePostgresCLIArg,
			ValueType: "numeric_mem", Value: sharedBuffers,
		}}}
	}
	fit := func(t *testing.T, ram int, g *parameters.ParameterGroup) error {
		t.Helper()
		meta := &InstanceMeta{Name: "qasphere", CPUCores: 2, RAMMB: ram, ParameterGroup: "qas-locks1024-20260729"}
		if _, err := fitInstanceSizes([]*InstanceMeta{meta}, 2, 7762); err != nil {
			t.Fatal(err)
		}
		return checkParameterGroupFits(meta, g)
	}

	// The ap1 group: 2 GB of shared_buffers is fine under the fitted 7762 MB.
	if err := fit(t, 8192, group("2 GB")); err != nil {
		t.Errorf("2 GB shared_buffers under 7762 MB: %v", err)
	}
	// A group sized by expression follows the fitted RAM down.
	if err := fit(t, 8192, &parameters.ParameterGroup{Parameters: parameters.GetDefaultParameters()}); err != nil {
		t.Errorf("the default group should fit any fitted size: %v", err)
	}

	// 7700 MB fits the recorded 8192 MB but not the fitted 7762 MB, so it is
	// the fit that refuses it, and it must refuse in preflight.
	if err := checkParameterGroupFits(&InstanceMeta{Name: "qasphere", CPUCores: 2, RAMMB: 8192}, group("7700 MB")); err != nil {
		t.Fatalf("7700 MB under the recorded 8192 MB should fit: %v", err)
	}
	err := fit(t, 8192, group("7700 MB"))
	if !errors.Is(err, parameters.ErrSharedMemoryTooLarge) || !errors.Is(err, operr.ErrInvalid) {
		t.Fatalf("err = %v, want ErrSharedMemoryTooLarge tagged invalid", err)
	}
	if !strings.Contains(err.Error(), "qas-locks1024-20260729") || !strings.Contains(err.Error(), "7762 MB") {
		t.Errorf("refusal should name the group and the fitted RAM: %v", err)
	}
}
