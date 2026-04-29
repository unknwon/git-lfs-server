package strx

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCoalesce(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{[]string{"a", "b"}, "a"},
		{[]string{"", "b", "c"}, "b"},
		{[]string{"", "", ""}, ""},
	}
	for _, test := range tests {
		t.Run(test.want, func(t *testing.T) {
			got := Coalesce(test.in...)
			assert.Equal(t, test.want, got)
		})
	}
}
