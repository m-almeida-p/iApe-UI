package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"iape-ui/internal/core/domain"
)

func TestPageRequest_Validate(t *testing.T) {
	tests := []struct {
		name string
		page domain.PageRequest
		ok   bool
	}{
		{"the zero value is the newest page", domain.PageRequest{}, true},
		{"before", domain.PageRequest{Before: "1795192195371278~conv_v1_x"}, true},
		{"after", domain.PageRequest{After: "42"}, true},
		{"the largest size", domain.PageRequest{Size: domain.MaxPageSize}, true},
		{"one more than the largest size", domain.PageRequest{Size: domain.MaxPageSize + 1}, false},
		{"a negative size", domain.PageRequest{Size: -1}, false},
		{"both directions", domain.PageRequest{Before: "1", After: "2"}, false},
		{"a cursor with a space", domain.PageRequest{Before: "1 2"}, false},
		{"a cursor with a newline", domain.PageRequest{After: "1\n2"}, false},
		{"a cursor with a character that is not ASCII", domain.PageRequest{Before: "1ñ"}, false},
		{"a cursor with a delete character", domain.PageRequest{Before: "1\x7f"}, false},
		{"a cursor at the limit", domain.PageRequest{Before: strings.Repeat("a", 256)}, true},
		{"a cursor over the limit", domain.PageRequest{Before: strings.Repeat("a", 257)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.page.Validate()

			if tc.ok {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, domain.ErrInvalidPage)
		})
	}
}
