// Package units parses and formats human sizes and durations ("26G", "14d").
package units

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseSize parses a byte size such as "512M", "26G", "1.5T" or a plain byte count.
// Suffixes are binary (K = 1024).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I")
	mult := float64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mult = 1 << 10
	case strings.HasSuffix(s, "M"):
		mult = 1 << 20
	case strings.HasSuffix(s, "G"):
		mult = 1 << 30
	case strings.HasSuffix(s, "T"):
		mult = 1 << 40
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(v * mult), nil
}

// FormatSize renders bytes with one binary suffix, e.g. 8589934592 -> "8G".
func FormatSize(b int64) string {
	const k = 1024
	switch {
	case b >= k*k*k*k:
		return trim(float64(b)/(k*k*k*k)) + "T"
	case b >= k*k*k:
		return trim(float64(b)/(k*k*k)) + "G"
	case b >= k*k:
		return trim(float64(b)/(k*k)) + "M"
	case b >= k:
		return trim(float64(b)/k) + "K"
	}
	return strconv.FormatInt(b, 10)
}

func trim(f float64) string {
	return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(f, 'f', 1, 64), "0"), ".")
}

// ParseDuration accepts Go durations plus a "d" (day) suffix.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(v * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

// ParseCPUList parses a cpuset list such as "0-14,16-30" into CPU numbers.
func ParseCPUList(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("invalid cpu list %q", s)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, fmt.Errorf("invalid cpu list %q", s)
			}
		}
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
	}
	return out, nil
}

// FormatCPUList renders sorted CPU numbers as a compact cpuset list.
func FormatCPUList(cpus []int) string {
	var parts []string
	for i := 0; i < len(cpus); {
		j := i
		for j+1 < len(cpus) && cpus[j+1] == cpus[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", cpus[i], cpus[j]))
		} else {
			parts = append(parts, strconv.Itoa(cpus[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}
