package ops

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"

	"github.com/narvidev/narvi/internal/platform"
)

// requiredConfigStages is every platform.Stage value RequiredConfigEnvVars
// actually boots platform.Load against to discover which variables it
// reports missing (§41.1 review round 2, findings Q4/Q5/Q8/Q9's own
// "production-relevant" wording). StageDevelopment is deliberately
// excluded: deploy/control-plane/secret.yaml is a PRODUCTION artifact --
// its own top comment says so -- and nothing it exists to fill in should
// be driven by whatever additionally-lenient behavior a development boot
// might one day grow (there isn't one today; this list should not depend
// on that staying true).
var requiredConfigStages = []platform.Stage{platform.StageStaging, platform.StageProduction}

// deploySecretExtraVars is the named, documented exception list this
// file's own top comment explains in full: variables platform.Config
// requires only once an operator opts into a feature by setting some
// OTHER variable first, which RequiredConfigEnvVars' single-pass, flat
// empty-environment probe cannot discover on its own -- see that
// function's own doc comment for exactly why. Kept here, not inferred, so
// a reviewer sees the exact, closed list rather than a scan whose
// "required" set silently grew a special case.
var deploySecretExtraVars = []string{
	// The allowlist group (§13.1): EmptyAllowlistError fires only when
	// all three are empty together, and carries no EnvVar field at all
	// (it is an empty struct) -- so no single one of them, and no group
	// name, is ever something RequiredConfigEnvVars' reflection-based
	// extraction can find.
	"NARVI_ALLOWED_EMAIL_DOMAINS",
	"NARVI_ALLOWED_GITHUB_ORGS",
	"NARVI_ALLOWED_EMAILS",
	// Object storage (§28.7): the whole bundle lives behind `if
	// objectStoreEndpoint != ""` in Load's own source. RequiredConfigEnvVars'
	// empty-environment probe never sets NARVI_OBJECT_STORE_ENDPOINT, so
	// that whole block -- REGION and BUCKET's own MissingRequiredEnvError
	// included -- never even runs; and the credential pair
	// (ACCESS_KEY_ID/SECRET_ACCESS_KEY) and PUBLIC_ENDPOINT/USE_PATH_STYLE
	// are optional even once the bundle IS enabled
	// (InvalidObjectStoreCredentialsError only fires on a MISMATCHED
	// pair, and it too carries no EnvVar field). All seven are added back
	// by hand so the template is usable against a real S3/MinIO bucket,
	// not just whatever this probe happens to trigger with everything
	// else left empty.
	"NARVI_OBJECT_STORE_ENDPOINT",
	"NARVI_OBJECT_STORE_REGION",
	"NARVI_OBJECT_STORE_BUCKET",
	"NARVI_OBJECT_STORE_ACCESS_KEY_ID",
	"NARVI_OBJECT_STORE_SECRET_ACCESS_KEY",
	"NARVI_OBJECT_STORE_PUBLIC_ENDPOINT",
	"NARVI_OBJECT_STORE_USE_PATH_STYLE",
}

// narviEnvVarLiteralPattern is the "read" half's own, deliberately crude,
// superset scan -- see RequiredConfigEnvVars' own doc comment for why a
// plain text scan is the right tool for this direction specifically.
var narviEnvVarLiteralPattern = regexp.MustCompile(`"(NARVI_[A-Z0-9_]+)"`)

// narviTokenPattern finds every bare NARVI_* token inside an
// already-rendered error message -- envVarNamesOf's own fallback path,
// below.
var narviTokenPattern = regexp.MustCompile(`NARVI_[A-Z0-9_]+`)

// RequiredConfigEnvVars is this repository's own drift guard (§41.1): the
// repository claims deploy/control-plane/secret.yaml names "every required
// platform.Config variable" (that file's own top comment, and this
// package's own doc.go house style of keeping a claim honest with a test
// rather than discipline).
//
// # Behavioral, not source-pattern-matched (§41.1 review round 2, findings Q4/Q5/Q8/Q9)
//
// An earlier version of this function was a go/ast walk over
// internal/platform/config.go's own SOURCE, hunting for the handful of
// shapes Load's own source used to say "this variable is missing":
// a MissingRequiredEnvError/InvalidHMACSecretError composite literal, or a
// fmt.Errorf whose format string said "required" or "missing". Every
// review round found another real shape it missed -- an unkeyed composite
// literal (MissingRequiredEnvError{x} compiles fine, and the scan's own
// "keyed field only" walk skipped it in silence), a fmt.Errorf built
// through a helper's own parameter instead of a literal identifier, a
// table-driven loop accumulating names into a []string before ever
// calling fmt.Errorf -- because a source-shape scanner can only recognize
// shapes someone thought to teach it, and "how do I signal a missing
// variable" is exactly the kind of thing a future contributor has no
// reason to know this file's scanner cares about.
//
// This version asks a different question entirely: not "does config.go's
// SOURCE look like it requires this variable," but "does platform.Load
// ACTUALLY refuse to boot without it." It calls platform.LoadWithLookup
// (internal/platform/config.go's own seam, added for exactly this) once
// per requiredConfigStages value, against a totally empty injected
// environment (Stage set, nothing else), and walks the errors that call
// actually returns -- via errors.Join's own Unwrap() []error shape --
// through envVarNamesOf's own two tiers: a field named EnvVar via
// reflection (MissingRequiredEnvError, InvalidHMACSecretError,
// InvalidObjectStoreMaxBytesError, and any FUTURE typed error Load starts
// returning that happens to expose one -- no list of type names for a new
// one to be missing from), and, for an error with no such field at all, a
// fallback that reads the NARVI_* token(s) directly out of the rendered
// error message itself, whenever that message says "required" or
// "missing". This is immune, by construction, to every shape named above:
// a composite literal (keyed or not), a fmt.Errorf built from a literal
// identifier, a helper function's own parameter, or a table-driven loop's
// accumulated slice all end up producing SOME error whose Error() text is
// what an operator would actually see on a failed boot -- and that text,
// not config.go's source, is what this scanner reads.
//
// Two known variable groups are still handled by hand, via
// deploySecretExtraVars (see its own doc comment): variables that only
// become required once an operator sets some OTHER variable first (the
// allowlist group, the object-store bundle) can't be discovered by a
// single flat, empty-environment probe -- reaching them would mean
// simulating every combination of feature flags Load's own branches can
// take, which is exactly the "re-implement Load's own control flow a
// second time" this design refuses (CLAUDE.md's own "no I/O... every
// state transition" convention, applied to this scanner too).
//
// read is still a plain, deliberately crude superset scan for every
// "NARVI_..." string literal appearing anywhere in configPath's raw
// source text (narviEnvVarLiteralPattern) -- TestDeploySecretTemplate's
// own "names a variable config.go no longer reads at all" direction only
// ever needs a superset (a literal env var name appearing in config.go's
// source at all, in any shape whatsoever), never a precise
// shape-by-shape classification the way "required" did, so a plain text
// scan is the right tool here and was never the fragile half of this
// file.
func RequiredConfigEnvVars(configPath string) (required map[string]bool, read map[string]bool, err error) {
	required = map[string]bool{}
	for _, stage := range requiredConfigStages {
		env := map[string]string{platform.StageEnvVarName: string(stage)}
		lookup := func(key string) (string, bool) {
			v, ok := env[key]
			return v, ok
		}
		if _, loadErr := platform.LoadWithLookup(lookup); loadErr != nil {
			for _, leaf := range flattenJoinedErrors(loadErr) {
				for _, name := range envVarNamesOf(leaf) {
					required[name] = true
				}
			}
		}
	}

	raw, readErr := os.ReadFile(configPath)
	if readErr != nil {
		return nil, nil, fmt.Errorf("ops: read %s: %w", configPath, readErr)
	}
	read = map[string]bool{}
	for _, m := range narviEnvVarLiteralPattern.FindAllStringSubmatch(string(raw), -1) {
		read[m[1]] = true
	}

	return required, read, nil
}

// flattenJoinedErrors recursively expands every errors.Join tree (Load's
// own Unwrap() []error shape) into its leaf errors, so envVarFieldOf below
// only ever has to look at one error at a time.
func flattenJoinedErrors(err error) []error {
	if err == nil {
		return nil
	}
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range u.Unwrap() {
			out = append(out, flattenJoinedErrors(e)...)
		}
		return out
	}
	return []error{err}
}

// envVarNamesOf extracts every NARVI_* variable name a single leaf error
// (already unwrapped from Load's own errors.Join tree by
// flattenJoinedErrors) names as missing, in two tiers:
//
//  1. Structured, via reflection on a field called EnvVar -- deliberately
//     not a hard-coded list of type names (see RequiredConfigEnvVars' own
//     doc comment for why): any error type platform.Load returns that
//     exposes a string field named EnvVar is picked up, with nothing here
//     needing to change the day Load starts returning a new one.
//     MissingRequiredEnvError, InvalidHMACSecretError, and
//     InvalidObjectStoreMaxBytesError all satisfy this today.
//  2. A fallback, for a "this is missing/required" error that carries NO
//     structured field at all -- a plain fmt.Errorf, however it was
//     built (a bare identifier, a helper function's own parameter, a
//     table-driven loop's accumulated []string joined into the message):
//     if the error's own rendered String() both mentions "required" or
//     "missing" AND contains an actual NARVI_* token, every such token is
//     extracted directly from the message text Load itself produced.
//     This is NOT a source-shape heuristic -- it never looks at
//     config.go's source at all -- it only reads the message an operator
//     would see on a real failed boot, which is necessarily the SAME
//     text regardless of which of the shapes above built it. A message
//     that says "required"/"missing" but names no NARVI_ variable (e.g.
//     EmptyAllowlistError's own "at least one of ... must be set", which
//     says neither word) is deliberately left to deploySecretExtraVars
//     instead of being guessed at here.
func envVarNamesOf(err error) []string {
	v := reflect.ValueOf(err)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			break
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		if f := v.FieldByName("EnvVar"); f.IsValid() && f.Kind() == reflect.String && f.String() != "" {
			return []string{f.String()}
		}
	}

	msg := err.Error()
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "required") && !strings.Contains(lower, "missing") {
		return nil
	}
	return narviTokenPattern.FindAllString(msg, -1)
}
