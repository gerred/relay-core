package image_test

import (
	"fmt"
	"testing"

	"github.com/puppetlabs/relay-core/pkg/util/image"
	"github.com/stretchr/testify/require"
)

func TestImageEntrypoint(t *testing.T) {
	tcs := []struct {
		Name     string
		Image    string
		Command  []string
		Args     []string
		Expected []string
	}{
		{
			Name:     "Image with defined ENTRYPOINT & COMMAND",
			Image:    "gcr.io/nebula-tasks/entrypoint",
			Command:  nil,
			Args:     nil,
			Expected: []string{"-entrypoint", "ni", "--", "log", "info", "hello world"},
		},
		{
			Name:     "Image with defined ENTRYPOINT & COMMAND and additional arguments",
			Image:    "gcr.io/nebula-tasks/entrypoint",
			Command:  nil,
			Args:     []string{"yes", "no"},
			Expected: []string{"-entrypoint", "ni", "--", "log", "info", "hello world", "yes", "no"},
		},
		{
			Name:     "Image with defined ENTRYPOINT & COMMAND and overridden command",
			Image:    "gcr.io/nebula-tasks/entrypoint",
			Command:  []string{"execute", "this"},
			Args:     nil,
			Expected: []string{"-entrypoint", "execute", "--", "this"},
		},
		{
			Name:     "Image with defined ENTRYPOINT & COMMAND, overridden command, and additional arguments",
			Image:    "gcr.io/nebula-tasks/entrypoint",
			Command:  []string{"execute", "this"},
			Args:     []string{"yes", "no"},
			Expected: []string{"-entrypoint", "execute", "--", "this", "yes", "no"},
		},
	}

	for _, test := range tcs {
		t.Run(fmt.Sprintf("%s", test.Name), func(t *testing.T) {
			result, err := image.ImageEntrypoint(test.Image, test.Command, test.Args)
			require.NoError(t, err)

			require.Equal(t, test.Expected, result)
		})
	}
}
