package remoteconfig

import "encoding/json"

// applyPatchToDocument applies patch (RFC 7386 JSON Merge Patch semantics,
// restricted by validation to the "control", "updater", "logging" and
// "defaultServer" top-level keys) on top of doc and returns a *new* Document
// - doc itself is never mutated, so callers can dry-run a patch and discard
// the result, or apply several overrides in sequence without them seeing
// each other's output.
//
// Implementation: round-trip doc to a generic JSON tree, merge-patch that
// tree, then decode the result back into a Document. This is simpler and
// less error-prone than hand-rolling per-field patch logic for every nested
// struct, at the cost of two JSON passes per override - negligible next to
// an HTTP round trip.
func applyPatchToDocument(doc *Document, patch PatchDoc) (*Document, error) {
	if len(patch) == 0 {
		return doc, nil
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var tree map[string]interface{}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}

	for key, value := range patch {
		if !AllowedPatchKeys[key] {
			// Already caught by validateOverridesShape; defensive no-op so a
			// caller that skips shape validation can't smuggle a write to an
			// arbitrary key through the dry-run path.
			continue
		}
		tree[key] = mergePatch(tree[key], value)
	}

	merged, err := json.Marshal(tree)
	if err != nil {
		return nil, err
	}
	var out Document
	if err := json.Unmarshal(merged, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// mergePatch implements RFC 7386 §2: if both target and patch are JSON
// objects, merge recursively (a null value in patch deletes the key);
// otherwise the patch value replaces the target wholesale (this covers
// arrays and scalars, and the case where patch itself is not an object).
func mergePatch(target, patch interface{}) interface{} {
	patchObj, patchIsObj := patch.(map[string]interface{})
	if !patchIsObj {
		return patch
	}

	targetObj, targetIsObj := target.(map[string]interface{})
	if !targetIsObj {
		targetObj = map[string]interface{}{}
	} else {
		// Don't mutate the caller's map in place.
		copied := make(map[string]interface{}, len(targetObj))
		for k, v := range targetObj {
			copied[k] = v
		}
		targetObj = copied
	}

	for k, v := range patchObj {
		if v == nil {
			delete(targetObj, k)
			continue
		}
		targetObj[k] = mergePatch(targetObj[k], v)
	}
	return targetObj
}
