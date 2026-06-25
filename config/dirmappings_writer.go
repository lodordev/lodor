package config

// Auto-generated directory_mappings writer — companion to WriteHostUpdate. When
// first-run onboarding writes host/auth/device but no directory_mappings, the mirror
// self-heals by generating them and persisting via this writer (preserving the host
// block byte-for-value). CGO-free, stdlib only.

import (
	"encoding/json"
	"fmt"
	"os"
)

// WriteDirectoryMappings persists the directory_mappings block to config.json
// atomically, preserving every other key (hosts/token/device_id, the launcher's own
// keys, fields this struct doesn't model) byte-for-value via the same generic-tree
// edit WriteHostUpdate uses. It sets ONLY the "directory_mappings" key from the
// supplied map; pass the FULL desired set (callers generate the whole block at once).
// An empty/nil map removes the key. Used by the mirror's lazy auto-generate
// self-heal when onboarding left no mappings.
//
// SECURITY: like WriteHostUpdate, this never reads, logs, or returns the token/host/
// device_id; errors name the failing step only, and the host block is left untouched.
func WriteDirectoryMappings(mappings map[string]DirMapping) error {
	path := configFileName

	var root map[string]any
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if uerr := json.Unmarshal(data, &root); uerr != nil {
			return fmt.Errorf("parse config: %w", uerr)
		}
		if root == nil {
			root = map[string]any{}
		}
	case os.IsNotExist(err):
		root = map[string]any{}
	default:
		return fmt.Errorf("read config: %w", err)
	}

	if len(mappings) == 0 {
		delete(root, "directory_mappings")
		return writeJSONAtomic(path, root)
	}

	// Marshal+unmarshal the typed map through JSON so the on-disk shape is exactly the
	// DirMapping json tags (slug/relative_path, omitempty honored) — keeping the
	// generated block byte-identical to a hand-baked one.
	dm := map[string]any{}
	for slug, m := range mappings {
		b, merr := json.Marshal(m)
		if merr != nil {
			return fmt.Errorf("encode mapping: %w", merr)
		}
		var entry any
		if uerr := json.Unmarshal(b, &entry); uerr != nil {
			return fmt.Errorf("encode mapping: %w", uerr)
		}
		dm[slug] = entry
	}
	root["directory_mappings"] = dm

	return writeJSONAtomic(path, root)
}
