// Package component holds the pure rules of catalog components
// (docs/design/component-usage.md): no I/O, plain inputs.
package component

import "reflect"

// PruneUnchanged returns the tenant's overrides: what sent (the values a
// tenant sent with enable or update, usually the definition's userValues file
// as edited) says that file does not. A key whose value equals the file's
// at the same path is dropped; maps are compared key by key, recursively;
// lists and other values are compared whole. A key the file does not have
// is kept, and so is an explicit null where the file has a value (it
// removes that value). A key the tenant deleted from the file is not
// stored: the file's value applies. So only real edits are stored, and a
// later change to the file reaches every tenant who did not edit that key.
// The price: a tenant cannot pin a value that equals today's file.
//
// Both documents must be decoded the same way (JSON, or YAML through
// sigs.k8s.io/yaml) so equal numbers compare equal. Neither is changed;
// the result is nil when nothing is left.
func PruneUnchanged(sent, file map[string]any) map[string]any {
	var out map[string]any
	for key, value := range sent {
		kept, keep := pruneValue(value, file, key)
		if !keep {
			continue
		}
		if out == nil {
			out = map[string]any{}
		}
		out[key] = kept
	}
	return out
}

// pruneValue is what of value (sent at key) is an edit of file[key], and
// whether anything is.
func pruneValue(value any, file map[string]any, key string) (any, bool) {
	original, inFile := file[key]
	if !inFile {
		return value, true
	}
	sentMap, sentIsMap := value.(map[string]any)
	fileMap, fileIsMap := original.(map[string]any)
	if sentIsMap && fileIsMap {
		pruned := PruneUnchanged(sentMap, fileMap)
		return pruned, pruned != nil
	}
	if reflect.DeepEqual(value, original) {
		return nil, false
	}
	return value, true
}
