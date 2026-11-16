package collection

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
)

// ContentHashComponents holds the per-domain hashes that compose a
// collection's global ContentHash.
type ContentHashComponents struct {
	RenderHash        string
	ScriptHash        string
	StatusPatchesHash string
}

// CombineContentHash produces a single deterministic hash from the
// per-domain components using domain separation. Each component must
// be a lowercase hex-encoded SHA-256 digest (64 chars).
func CombineContentHash(c ContentHashComponents) (string, error) {
	if err := validateHex64(c.RenderHash, "RenderHash"); err != nil {
		return "", err
	}
	if err := validateHex64(c.ScriptHash, "ScriptHash"); err != nil {
		return "", err
	}
	if err := validateHex64(c.StatusPatchesHash, "StatusPatchesHash"); err != nil {
		return "", err
	}

	h := sha256.New()
	for _, line := range []string{
		"kit.collection.content.v1\n",
		"render " + c.RenderHash + "\n",
		"scripts " + c.ScriptHash + "\n",
		"statusPatches " + c.StatusPatchesHash + "\n",
	} {
		if _, err := h.Write([]byte(line)); err != nil {
			return "", fmt.Errorf("CombineContentHash: hashing: %w", err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validateHex64(s, field string) error {
	if len(s) != 64 {
		return fmt.Errorf("CombineContentHash: %s must be 64 hex chars, got %d", field, len(s))
	}
	if s != strings.ToLower(s) {
		return fmt.Errorf("CombineContentHash: %s must be lowercase hex", field)
	}
	if _, err := hex.DecodeString(s); err != nil {
		return fmt.Errorf("CombineContentHash: %s is not valid hex: %w", field, err)
	}
	return nil
}

// HashStatusPatches computes a canonical hash over the declared
// StatusPatch entries, not their evaluated results. A change in
// Values that only affects a CEL expression inside a patch will not
// change this hash; Agent-level values already flow through
// BundleDigest. Order is preserved because application order matters.
// Each patch is canonicalized via sorted-key JSON.
func HashStatusPatches(patches []v1alpha1.StatusPatch) (string, error) {
	h := sha256.New()
	for i, sp := range patches {
		canonical, err := canonicalJSON(sp)
		if err != nil {
			return "", fmt.Errorf("canonicalizing status patch %d (%s/%s): %w", i, sp.Kind, sp.Name, err)
		}
		h.Write(canonical)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalJSON produces sorted-key, compact JSON for a value.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	return marshalSorted(generic)
}

func marshalSorted(v any) ([]byte, error) {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyBytes, _ := json.Marshal(k)
			buf.Write(keyBytes)
			buf.WriteByte(':')
			valBytes, err := marshalSorted(val[k])
			if err != nil {
				return nil, err
			}
			buf.Write(valBytes)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			itemBytes, err := marshalSorted(item)
			if err != nil {
				return nil, err
			}
			buf.Write(itemBytes)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	default:
		return json.Marshal(v)
	}
}
