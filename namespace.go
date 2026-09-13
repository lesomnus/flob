package flob

import (
	"encoding/base64"
	"errors"
	"strings"
)

// namespaceSegment preserves conventional names and encodes every other ID into
// one reversible path segment. The '~' prefix cannot occur in preserved names.
func namespaceSegment(id string) string {
	if plainNamespace(id) {
		return id
	}
	return "~" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func plainNamespace(id string) bool {
	if id == "" || strings.HasSuffix(id, ".") {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	// Windows reserves these names even when followed by an extension.
	base, _, _ := strings.Cut(strings.ToUpper(id), ".")
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return false
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return false
	}
	return true
}

func namespaceID(segment string) (string, error) {
	if encoded, ok := strings.CutPrefix(segment, "~"); ok {
		data, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return "", err
		}
		if namespaceSegment(string(data)) != segment {
			return "", errors.New("noncanonical namespace encoding")
		}
		return string(data), nil
	}
	return segment, nil
}
