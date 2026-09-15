package config

import (
	"errors"
	"net/http"
	"strings"

	"golang.org/x/net/http/httpguts"
)

var (
	ErrInvalidHeaderName   = errors.New("invalid header name")
	ErrInvalidHeaderValue  = errors.New("invalid header value")
	ErrDuplicateHeaderName = errors.New("duplicate header name")
)

// NormalizeProviderHeaders validates an HTTP header map, canonicalizes names,
// and returns an independent map. A nil map remains nil; an allocated empty map
// remains allocated so callers can distinguish an omitted value from a clear.
func NormalizeProviderHeaders(headers map[string]string) (map[string]string, error) {
	if headers == nil {
		return nil, nil
	}

	normalized := make(map[string]string, len(headers))
	seen := make(map[string]struct{}, len(headers))
	for name, value := range headers {
		if !httpguts.ValidHeaderFieldValue(value) {
			return nil, ErrInvalidHeaderValue
		}
		name = strings.Trim(name, " \t")
		if name == "" || !httpguts.ValidHeaderFieldName(name) {
			return nil, ErrInvalidHeaderName
		}
		canonical := http.CanonicalHeaderKey(name)
		folded := strings.ToLower(canonical)
		if _, duplicate := seen[folded]; duplicate {
			return nil, ErrDuplicateHeaderName
		}
		seen[folded] = struct{}{}
		normalized[canonical] = strings.Trim(value, " \t")
	}
	return normalized, nil
}
