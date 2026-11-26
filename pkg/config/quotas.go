package config

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
)

// QuotasConfig configures the quota catalog (NEBIUS_MOCKER_QUOTAS_*).
type QuotasConfig struct {
	// Limits overrides the default limit of catalog quotas: quota name →
	// limit of every tenant and project allowance nobody has set. Set with
	// NEBIUS_MOCKER_QUOTAS_LIMITS="compute.instance.count=10,compute.disk.size.network-ssd=4Ti".
	Limits map[string]uint64
}

func quotasParsers(q *QuotasConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"QUOTAS_LIMITS", func(s string) error {
			limits, err := parseQuotaLimits(s)
			q.Limits = limits
			return err
		}},
	}
}

// parseQuotaLimits parses "name=value,…"; values accept a Ki/Mi/Gi/Ti/Pi
// suffix (powers of 1024, for byte quotas).
func parseQuotaLimits(s string) (map[string]uint64, error) {
	limits := map[string]uint64{}
	for _, item := range splitList(s) {
		name, value, ok := strings.Cut(item, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("want name=limit pairs such as compute.instance.count=10, got %q", item)
		}
		v, err := parseQuotaValue(value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		limits[name] = v
	}
	return limits, nil
}

func parseQuotaValue(s string) (uint64, error) {
	shift := uint(0)
	num := s
	for i, suffix := range []string{"Ki", "Mi", "Gi", "Ti", "Pi"} {
		if v, ok := strings.CutSuffix(s, suffix); ok {
			num, shift = v, uint(10*(i+1))
			break
		}
	}
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("want a non-negative integer with an optional Ki/Mi/Gi/Ti/Pi suffix, got %q", s)
	}
	if shift > 0 && bits.LeadingZeros64(n) < int(shift) {
		return 0, fmt.Errorf("%q overflows uint64", s)
	}
	return n << shift, nil
}
