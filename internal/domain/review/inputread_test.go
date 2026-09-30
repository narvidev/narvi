package review_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
)

func TestInputRead_Readable(t *testing.T) {
	tests := []struct {
		read review.InputRead
		want bool
	}{
		{review.InputReadComplete, true},
		{review.InputReadEmpty, true},
		{review.InputReadNotFetched, false},
		{review.InputReadPRUnreadable, false},
		{review.InputReadDiffUnreadable, false},
		{review.InputReadDiffTruncated, false},
		{"", false},
		{"garbled", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.read), func(t *testing.T) {
			if got := tt.read.Readable(); got != tt.want {
				t.Errorf("InputRead(%q).Readable() = %v, want %v", tt.read, got, tt.want)
			}
		})
	}
}
