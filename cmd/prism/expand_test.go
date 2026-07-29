package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		input string
		want  string
	}{
		{"~/foo/bar", filepath.Join(home, "foo/bar")},
		{"~/", home},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		{"", ""},
		{"~noslash", "~noslash"},
		{"~", "~"},
	}
	for _, tt := range tests {
		got, err := expandHome(tt.input)
		if err != nil {
			t.Errorf("expandHome(%q) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("expandHome(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
