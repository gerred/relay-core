package fnlib

import "github.com/puppetlabs/relay-core/pkg/expr/fn"

var (
	library = map[string]fn.Descriptor{
		"append":          appendDescriptor,
		"concat":          concatDescriptor,
		"convertMarkdown": convertMarkdownDescriptor,
		"equals":          equalsDescriptor,
		"jsonUnmarshal":   jsonUnmarshalDescriptor,
		"merge":           mergeDescriptor,
		"notEquals":       notEqualsDescriptor,
	}
)

// Library creates an fn.Map of all the core functions supported
// by the platform.
func Library() fn.Map {
	return fn.NewMap(library)
}
