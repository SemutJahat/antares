package config

import (
	"errors"
	"reflect"
	"testing"
)

func TestNormalizeProviderHeaders(t *testing.T) {
	input := map[string]string{" x-tenant ": " team=a=b ", "Empty": ""}
	got, err := NormalizeProviderHeaders(input)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"X-Tenant": "team=a=b", "Empty": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized = %#v, want %#v", got, want)
	}
	got["X-Tenant"] = "changed"
	if input[" x-tenant "] != " team=a=b " {
		t.Fatalf("normalization mutated input: %#v", input)
	}

	nilHeaders, err := NormalizeProviderHeaders(nil)
	if err != nil || nilHeaders != nil {
		t.Fatalf("nil headers = %#v, %v; want nil, nil", nilHeaders, err)
	}
	emptyHeaders, err := NormalizeProviderHeaders(map[string]string{})
	if err != nil || emptyHeaders == nil || len(emptyHeaders) != 0 {
		t.Fatalf("empty headers = %#v, %v; want allocated empty map", emptyHeaders, err)
	}
}

func TestNormalizeProviderHeadersRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    error
	}{
		{"invalid name", map[string]string{"bad header": "value"}, ErrInvalidHeaderName},
		{"control value", map[string]string{"X-Test": "bad\nvalue"}, ErrInvalidHeaderValue},
		{"duplicate casing", map[string]string{"X-Test": "one", "x-test": "two"}, ErrDuplicateHeaderName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeProviderHeaders(tc.headers)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}
