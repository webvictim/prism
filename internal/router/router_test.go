package router

import "testing"

func TestIsOpenAIPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/v1/chat/completions", true},
		{"/v1/chat/completions/", true},
		{"/v1/models", true},
		{"/v1/models/gpt-4", true},
		{"/v1/embeddings", true},
		{"/v1/completions", true},
		{"/v1/messages", false},
		{"/v1/messages/batch", false},
		{"/_prism/health", false},
		{"/", false},
		{"/v1/complete", false},
	}
	for _, tt := range tests {
		if got := isOpenAIPath(tt.path); got != tt.want {
			t.Errorf("isOpenAIPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
