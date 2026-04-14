package store

import "testing"

func TestAppendToStringList(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		value     string
		want      string
		wantAdded bool
	}{
		{
			name:      "empty raw creates new array",
			raw:       "",
			value:     "abc123",
			want:      `["abc123"]`,
			wantAdded: true,
		},
		{
			name:      "append to existing array",
			raw:       `["abc123"]`,
			value:     "def456",
			want:      `["abc123","def456"]`,
			wantAdded: true,
		},
		{
			name:      "dedup skips existing value",
			raw:       `["abc123","def456"]`,
			value:     "abc123",
			want:      `["abc123","def456"]`,
			wantAdded: false,
		},
		{
			name:      "invalid JSON starts fresh",
			raw:       "not-json",
			value:     "abc123",
			want:      `["abc123"]`,
			wantAdded: true,
		},
		{
			name:      "wrong JSON type starts fresh",
			raw:       `{"key":"val"}`,
			value:     "abc123",
			want:      `["abc123"]`,
			wantAdded: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, added := appendToStringList(tt.raw, tt.value)
			if got != tt.want {
				t.Errorf("appendToStringList() = %q, want %q", got, tt.want)
			}
			if added != tt.wantAdded {
				t.Errorf("appendToStringList() added = %v, want %v", added, tt.wantAdded)
			}
		})
	}
}
