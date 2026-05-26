package config

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// redactedMarker is the placeholder value stamped over any field whose name
// or position marks it as a secret. The marker keeps the *presence* of the
// secret visible (so an operator can confirm "yes, this is set") without
// revealing the value.
const redactedMarker = "<redacted>"

// secretKeyNames lists the lowercase mapping-key names that, when found
// inside the opaque provider_config blob, mark a value as a secret. Matching
// is case-insensitive and exact on the key (no substring matching — that
// would over-redact harmless fields like "tokenizer" or "secretly_safe").
//
// The list is intentionally conservative: every cloud provider's SDK
// already documents the conventional names for credential fields, and these
// cover the in-tree providers (Linode's `token`, anything we plumb later
// that uses `api_key`/`secret_key`).
var secretKeyNames = map[string]struct{}{
	"token":      {},
	"password":   {},
	"secret":     {},
	"key":        {},
	"api_key":    {},
	"access_key": {},
	"secret_key": {},
}

// Redact returns a copy of cfg with every secret-bearing field zeroed out
// (replaced with "<redacted>"). The result is safe to ship over the
// operator-facing control plane.
//
// Today the redacted fields are:
//
//   - Forgejo.Token — the admin token used to poll the queue. The
//     redacted copy keeps the field present so the operator can confirm
//     it was set without seeing the value.
//   - ProviderConfig — the opaque provider blob, walked recursively. Any
//     mapping value whose key matches one of secretKeyNames is replaced
//     with "<redacted>". Non-secret keys (region, type, image, IDs,
//     CIDRs, …) pass through unchanged.
//
// SSH.PrivateKeyFile is intentionally NOT redacted: the *path* is operator
// configuration, not a secret. The file it points at is the secret.
func Redact(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	out := *cfg // shallow copy: defaults + scalars + slices we don't mutate
	if out.Forgejo.Token != "" {
		out.Forgejo.Token = redactedMarker
	}
	out.ProviderConfig = redactNode(cfg.ProviderConfig)
	return &out
}

// redactNode returns a deep-copied yaml.Node tree with every secret value
// replaced. Recurses into mappings and sequences; scalars and aliases are
// returned by value. The input is never mutated.
func redactNode(n yaml.Node) yaml.Node {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		out := n
		out.Content = make([]*yaml.Node, len(n.Content))
		for i, c := range n.Content {
			cp := redactNode(*c)
			out.Content[i] = &cp
		}
		return out
	case yaml.MappingNode:
		out := n
		out.Content = make([]*yaml.Node, len(n.Content))
		// Mapping content is alternating key, value, key, value, ...
		for i := 0; i < len(n.Content); i += 2 {
			keyNode := *n.Content[i]
			out.Content[i] = &keyNode
			if i+1 >= len(n.Content) {
				break
			}
			valNode := *n.Content[i+1]
			if isSecretKey(keyNode.Value) && valNode.Kind == yaml.ScalarNode {
				valNode.Value = redactedMarker
				valNode.Tag = "!!str"
				valNode.Style = 0
			} else {
				valNode = redactNode(valNode)
			}
			out.Content[i+1] = &valNode
		}
		return out
	case yaml.ScalarNode, yaml.AliasNode:
		// Scalars and aliases never carry nested secrets we can locate
		// by name — they pass through unchanged.
		return n
	default:
		return n
	}
}

// isSecretKey reports whether a mapping key name (case-insensitive) marks a
// value as a secret per secretKeyNames.
func isSecretKey(name string) bool {
	_, ok := secretKeyNames[strings.ToLower(name)]
	return ok
}
