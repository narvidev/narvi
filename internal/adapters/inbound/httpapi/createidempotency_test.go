package httpapi

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
)

// TestParseIdempotencyKey_Table is technical plan §43.8's key format on
// REST: exactly what format:uuid and the MCP tool's schema check accept --
// 8-4-4-4-12 hexadecimal digits in either case, both cases one key -- and
// nothing else, not even the spellings pgtype would read as a UUID.
func TestParseIdempotencyKey_Table(t *testing.T) {
	const lower = "01234567-89ab-4def-8123-456789abcdef"
	var want [16]byte
	copy(want[:], []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0x4d, 0xef, 0x81, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef})

	for _, key := range []string{lower, strings.ToUpper(lower), "01234567-89AB-4def-8123-456789ABcdef"} {
		t.Run("accepts "+key, func(t *testing.T) {
			got, err := parseIdempotencyKey(restdtos.CreateSessionRequest{IdempotencyKey: &key})
			if err != nil || !got.Valid || got.Bytes != want {
				t.Fatalf("parse %q = %v (err %v), want the one key %x", key, got, err, want)
			}
		})
	}

	for _, key := range []string{
		"",
		"not-a-uuid",
		"0123456789ab4def8123456789abcdef",       // no hyphens: pgtype reads it
		"01234567x89abx4defx8123x456789abcdef",   // other separators: pgtype reads it
		"{01234567-89ab-4def-8123-456789abcdef}", // braces
		"urn:uuid:01234567-89ab-4def-8123-456789abcdef",
		" 01234567-89ab-4def-8123-456789abcdef",
		"01234567-89ab-4def-8123-456789abcdef ",
		"01234567-89ab-4def-8123-456789abcdeg",
		"01234567-89ab-4def-8123-456789abcde",
		"01234567-89ab-4def-8123-456789abcdef0",
		"0123456-789ab-4def-8123-456789abcdef",
		"01234567-89ab-4def-8123-456789abcdé",
	} {
		t.Run("refuses "+key, func(t *testing.T) {
			got, err := parseIdempotencyKey(restdtos.CreateSessionRequest{IdempotencyKey: &key})
			if err == nil || got.Valid || err.Error() != "idempotencyKey: must be a UUID, as 8-4-4-4-12 hexadecimal digits" {
				t.Fatalf("parse %q = %v (err %v), want the 400's refusal", key, got, err)
			}
		})
	}

	got, err := parseIdempotencyKey(restdtos.CreateSessionRequest{})
	if err != nil || got.Valid {
		t.Fatalf("no key = %v (err %v), want none and no error", got, err)
	}
}

// createHashBody is a create request body: the fields every request here
// carries, then extra (a JSON fragment starting with a comma, or empty).
func createHashBody(extra string) string {
	return `{"spawnSource":"web","title":null,"prompt":"fix it","repos":[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}],"modelId":null,"effort":null,"planMode":false` + extra + `}`
}

// hashOfBody decodes body as POST /api/sessions does and returns
// createRequestSHA256 of it.
func hashOfBody(t *testing.T, body string) []byte {
	t.Helper()
	var req restdtos.CreateSessionRequest
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	sum, err := createRequestSHA256(req)
	if err != nil {
		t.Fatalf("hash %s: %v", body, err)
	}
	return sum
}

// TestCreateRequestSHA256_Table is §43.8's replay comparison: a hash of the
// decoded request's canonical form, never of its bytes. Two spellings of
// one request -- keys in another order, other whitespace, an unknown field,
// another key or none, an optional field absent or given the value the
// route reads the same way -- hash alike; two requests the route would
// serve differently never do.
func TestCreateRequestSHA256_Table(t *testing.T) {
	base := createHashBody(`,"idempotencyKey":"0f6c1d2e-3a4b-4c5d-8e9f-a0b1c2d3e4f5"`)
	twoRepos := func(first, second string) string {
		return `{"spawnSource":"web","title":null,"prompt":"fix it","repos":[{"name":"` + first + `","url":"https://github.com/acme/` + first + `","branch":null},{"name":"` + second + `","url":"https://github.com/acme/` + second + `","branch":null}],"modelId":null,"effort":null,"planMode":false}`
	}
	tests := []struct {
		name      string
		a, b      string
		wantEqual bool
	}{
		// One request, spelled differently.
		{"keys in another order, other whitespace", base, "{\n  \"planMode\": false, \"idempotencyKey\": \"0f6c1d2e-3a4b-4c5d-8e9f-a0b1c2d3e4f5\",\t\"effort\": null, \"modelId\": null,\n  \"repos\": [ { \"branch\": null, \"url\": \"https://github.com/acme/widgets\", \"name\": \"widgets\" } ],\n  \"prompt\": \"fix it\", \"title\": null, \"spawnSource\": \"web\"\n}", true},
		{"an unknown field", base, createHashBody(`,"idempotencyKey":"0f6c1d2e-3a4b-4c5d-8e9f-a0b1c2d3e4f5","somethingElse":{"x":1}`), true},
		{"the key in capitals", base, createHashBody(`,"idempotencyKey":"0F6C1D2E-3A4B-4C5D-8E9F-A0B1C2D3E4F5"`), true},
		{"another key", base, createHashBody(`,"idempotencyKey":"11111111-2222-4333-8444-555555555555"`), true},
		{"no key", base, createHashBody(``), true},
		{"docker false", base, createHashBody(`,"docker":false`), true},
		{"pathScope null", base, createHashBody(`,"pathScope":null`), true},
		{"pathScope empty", base, createHashBody(`,"pathScope":[]`), true},
		{"mockConfig null", base, createHashBody(`,"mockConfig":null`), true},
		{"optional nullables null", base, createHashBody(`,"buildModelId":null,"buildEffort":null,"epistemicCheckEnabled":null,"egressPolicy":null`), true},
		{"every default at once", base, createHashBody(`,"docker":false,"pathScope":[],"mockConfig":null,"buildModelId":null,"buildEffort":null,"epistemicCheckEnabled":null`), true},
		{"mockConfig with no contracts path, and with the default one", createHashBody(`,"mockConfig":{}`), createHashBody(`,"mockConfig":{"contractsPath":"contracts/api"}`), true},
		{"mockConfig with a null contracts path, and with none", createHashBody(`,"mockConfig":{"contractsPath":null}`), createHashBody(`,"mockConfig":{}`), true},
		{"an empty allowlist, and a null one", createHashBody(`,"egressPolicy":{"mode":"open","allowlist":[]}`), createHashBody(`,"egressPolicy":{"mode":"open","allowlist":null}`), true},

		// Different requests.
		{"another prompt", base, strings.Replace(base, `"fix it"`, `"fix it again"`, 1), false},
		{"a title", base, strings.Replace(base, `"title":null`, `"title":"Fix"`, 1), false},
		{"a model", base, strings.Replace(base, `"modelId":null`, `"modelId":"anthropic/claude"`, 1), false},
		{"an effort", base, strings.Replace(base, `"effort":null`, `"effort":"high"`, 1), false},
		{"plan mode", base, strings.Replace(base, `"planMode":false`, `"planMode":true`, 1), false},
		{"a branch", base, strings.Replace(base, `"branch":null`, `"branch":"main"`, 1), false},
		{"another repository", base, strings.Replace(base, `acme/widgets`, `acme/gadgets`, 1), false},
		{"repositories in another order", twoRepos("widgets", "gadgets"), twoRepos("gadgets", "widgets"), false},
		{"a build model", base, createHashBody(`,"buildModelId":"anthropic/claude"`), false},
		{"a build effort", base, createHashBody(`,"buildEffort":"high"`), false},
		{"the epistemic check off, not the default", base, createHashBody(`,"epistemicCheckEnabled":false`), false},
		{"the epistemic check on", base, createHashBody(`,"epistemicCheckEnabled":true`), false},
		{"docker", base, createHashBody(`,"docker":true`), false},
		{"a path scope", base, createHashBody(`,"pathScope":["src/**"]`), false},
		{"path scopes in another order", createHashBody(`,"pathScope":["a/**","b/**"]`), createHashBody(`,"pathScope":["b/**","a/**"]`), false},
		{"a mockConfig", base, createHashBody(`,"mockConfig":{}`), false},
		{"another contracts path", createHashBody(`,"mockConfig":{}`), createHashBody(`,"mockConfig":{"contractsPath":"api"}`), false},
		{"an open egress policy", base, createHashBody(`,"egressPolicy":{"mode":"open","allowlist":[]}`), false},
		{"another egress mode", createHashBody(`,"egressPolicy":{"mode":"open","allowlist":[]}`), createHashBody(`,"egressPolicy":{"mode":"allowlist","allowlist":[]}`), false},
		{"an allowlist entry", createHashBody(`,"egressPolicy":{"mode":"allowlist","allowlist":[]}`), createHashBody(`,"egressPolicy":{"mode":"allowlist","allowlist":["example.com"]}`), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := hashOfBody(t, tc.a), hashOfBody(t, tc.b)
			if len(a) != 32 || len(b) != 32 {
				t.Fatalf("hash lengths %d and %d, want SHA-256's 32", len(a), len(b))
			}
			if bytes.Equal(a, b) != tc.wantEqual {
				t.Fatalf("hashes equal = %v, want %v\n a: %s\n b: %s", !tc.wantEqual, tc.wantEqual, tc.a, tc.b)
			}
		})
	}
}

// jsonFieldNames is every JSON field name t's struct fields encode as.
func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestCreateRequestFingerprint_CoversTheRequest: the canonical form a replay
// is compared by names every field of CreateSessionRequest but the two it
// leaves out on purpose (createRequestFingerprint's doc comment says why),
// and every field of the repositories and the egress policy -- so a field
// added to the request is either hashed or listed here as left out, never
// dropped from the comparison unnoticed, which would let two different
// requests replay each other.
func TestCreateRequestFingerprint_CoversTheRequest(t *testing.T) {
	leftOut := []string{"idempotencyKey", "spawnSource"}
	var request []string
	for _, name := range jsonFieldNames(reflect.TypeFor[restdtos.CreateSessionRequest]()) {
		if !slices.Contains(leftOut, name) {
			request = append(request, name)
		}
	}
	if got := jsonFieldNames(reflect.TypeFor[createRequestFingerprint]()); !slices.Equal(got, request) {
		t.Fatalf("fingerprint fields %v, want the request's %v less %v", got, request, leftOut)
	}
	if got, want := jsonFieldNames(reflect.TypeFor[createRequestFingerprintRepo]()), jsonFieldNames(reflect.TypeFor[restdtos.CreateSessionRequestReposElem]()); !slices.Equal(got, want) {
		t.Fatalf("fingerprint repository fields %v, want the request's %v", got, want)
	}
	if got, want := jsonFieldNames(reflect.TypeFor[createRequestFingerprintEgress]()), jsonFieldNames(reflect.TypeFor[restdtos.CreateSessionRequestEgressPolicy]()); !slices.Equal(got, want) {
		t.Fatalf("fingerprint egress fields %v, want the request's %v", got, want)
	}
	// mockConfig is hashed as the contracts path it resolves to: that must
	// stay its one field.
	if got := jsonFieldNames(reflect.TypeFor[restdtos.CreateSessionRequestMockConfig]()); !slices.Equal(got, []string{"contractsPath"}) {
		t.Fatalf("mockConfig fields %v: the fingerprint hashes contractsPath alone", got)
	}
}
