package util

import (
	"testing"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		name  string
		input int64
		want  string
	}{
		{"zero bytes", 0, "0 B"},
		{"bytes", 500, "500 B"},
		{"kilobytes", 1024, "1.0 KB"},
		{"kilobytes decimal", 1536, "1.5 KB"},
		{"megabytes", 1048576, "1.0 MB"},
		{"megabytes decimal", 1572864, "1.5 MB"},
		{"gigabytes", 1073741824, "1.0 GB"},
		{"gigabytes decimal", 1610612736, "1.5 GB"},
		{"terabytes", 1099511627776, "1.0 TB"},
		{"petabytes", 1125899906842624, "1.0 PB"},
		{"exabytes", 1152921504606846976, "1.0 EB"},
		{"large value", 9223372036854775807, "8.0 EB"}, // Max int64
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatBytes(tt.input)
			if got != tt.want {
				t.Errorf("FormatBytes(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
