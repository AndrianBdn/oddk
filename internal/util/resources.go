package util

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

func ParseRAMString(ramStr string) (int, error) {
	if ramStr == "" {
		return 0, fmt.Errorf("RAM value cannot be empty")
	}

	ramStr = strings.TrimSpace(ramStr)

	upperStr := strings.ToUpper(ramStr)
	var numStr string
	switch {
	case strings.HasSuffix(upperStr, "MIB"):
		numStr = strings.TrimSuffix(strings.TrimSuffix(upperStr, "MIB"), " ")
	case strings.HasSuffix(upperStr, "MB"):
		numStr = strings.TrimSuffix(strings.TrimSuffix(upperStr, "MB"), " ")
	case strings.HasSuffix(upperStr, "M"):
		numStr = strings.TrimSuffix(strings.TrimSuffix(upperStr, "M"), " ")
	default:
		ramGB, err := strconv.Atoi(ramStr)
		if err != nil {
			return 0, fmt.Errorf("invalid RAM value: %s", ramStr)
		}
		return ramGB * 1024, nil
	}

	ramMB, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, fmt.Errorf("invalid RAM value: %s", ramStr)
	}
	return ramMB, nil
}

func ValidateBasicResourceBounds(cpuCores, ramMB int) error {
	if cpuCores < 1 || cpuCores > 1024 {
		return fmt.Errorf("CPU cores must be between 1 and 1024")
	}
	if ramMB < 128 || ramMB > 1048576 { // 128MB to 1TB
		return fmt.Errorf("RAM must be between 128MB and 1TB")
	}
	return nil
}

func ValidateSystemResources(cpuCores, ramMB int) error {
	totalCores, err := cpu.Counts(true)
	if err != nil {
		return fmt.Errorf("failed to get CPU count: %w", err)
	}

	if totalCores == 0 {
		return fmt.Errorf("no CPU information available")
	}

	if cpuCores > totalCores {
		return fmt.Errorf("requested CPU cores (%d) exceeds available cores (%d)", cpuCores, totalCores)
	}

	if cpuCores <= 0 {
		return fmt.Errorf("CPU cores must be greater than 0")
	}

	memInfo, err := mem.VirtualMemory()
	if err != nil {
		return fmt.Errorf("failed to get memory information: %w", err)
	}

	totalMemBytes := memInfo.Total / (1024 * 1024)
	if totalMemBytes > uint64(int64(^uint(0)>>1)) {
		return fmt.Errorf("system memory too large for platform")
	}
	totalMemMB := int(totalMemBytes)
	if ramMB > totalMemMB {
		return fmt.Errorf("requested RAM (%d MB) exceeds available memory (%d MB)", ramMB, totalMemMB)
	}

	if ramMB <= 0 {
		return fmt.Errorf("RAM must be greater than 0 MB")
	}

	return nil
}

func GetSystemResourceLimits() (int, int, error) {
	totalCores, err := cpu.Counts(true)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get CPU count: %w", err)
	}

	if totalCores == 0 {
		return 0, 0, fmt.Errorf("no CPU information available")
	}

	memInfo, err := mem.VirtualMemory()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get memory information: %w", err)
	}

	totalMemBytes := memInfo.Total / (1024 * 1024)
	if totalMemBytes > uint64(int64(^uint(0)>>1)) {
		return 0, 0, fmt.Errorf("system memory too large for platform")
	}
	totalMemMB := int(totalMemBytes)

	return totalCores, totalMemMB, nil
}
