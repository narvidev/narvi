package ops

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
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
// file's own top comment explains in full: variables RequiredConfigEnvVars'
// two behavioral probes (the empty-environment probe and the single-
// variable presence probe -- see RequiredConfigEnvVars' own doc comment)
// cannot discover on their own, because platform.Load only requires them
// under a condition neither probe ever produces: a SPECIFIC value of some
// other variable (not just its bare presence), or two-or-more variables'
// joint state. Kept here, not inferred, so a reviewer sees the exact,
// closed list rather than a scan whose "required" set silently grew a
// special case.
var deploySecretExtraVars = []string{
	// The allowlist group (§13.1): EmptyAllowlistError fires only when
	// all three are empty TOGETHER -- an OR across three variables'
	// joint state, not any single variable's presence -- so it belongs
	// to this list's "two [or more] variables at once" class, never the
	// "gated on one variable's presence" class the single-variable
	// presence probe covers behaviorally. It also carries no EnvVar
	// field at all (it is an empty struct), so even if it WERE gated on
	// just one variable, envVarNamesOf's structured tier could not name
	// which one by reflection alone.
	"NARVI_ALLOWED_EMAIL_DOMAINS",
	"NARVI_ALLOWED_GITHUB_ORGS",
	"NARVI_ALLOWED_EMAILS",
	// Object storage (§28.7): the whole bundle is feature-flagged on
	// `if objectStoreEndpoint != ""` alone -- a single variable's plain
	// presence -- so REQUIRED-ness inside that block (REGION and
	// BUCKET's own MissingRequiredEnvError) is exactly what
	// RequiredConfigEnvVars' single-variable presence probe (below) now
	// discovers on its own, and REGION/BUCKET are deliberately NOT
	// repeated in this hand-listed exception list. The four vars that
	// ARE still listed below are not discoverable by ANY probe, because
	// none of them is ever itself the subject of a MissingRequiredEnvError:
	// ACCESS_KEY_ID/SECRET_ACCESS_KEY are validated as a MATCHED PAIR
	// (InvalidObjectStoreCredentialsError, gated on two variables' joint
	// state, and itself carries no EnvVar field), and
	// PUBLIC_ENDPOINT/USE_PATH_STYLE are unconditionally optional even
	// once the bundle is on. ENDPOINT itself is the presence-gate, never
	// required either. All four are added back by hand so the template
	// stays usable against a real S3/MinIO bucket, not just whatever the
	// probes below happen to trigger with everything else left empty.
	"NARVI_OBJECT_STORE_ENDPOINT",
	"NARVI_OBJECT_STORE_ACCESS_KEY_ID",
	"NARVI_OBJECT_STORE_SECRET_ACCESS_KEY",
	"NARVI_OBJECT_STORE_PUBLIC_ENDPOINT",
	"NARVI_OBJECT_STORE_USE_PATH_STYLE",
	// OIDC sign-in (§41.3): NARVI_OIDC_ISSUER/CLIENT_ID/
	// CLIENT_SECRET are all-or-none (InvalidOIDCConfigError, gated on all
	// three variables' joint state, and -- like
	// InvalidObjectStoreCredentialsError immediately above -- itself
	// carries no EnvVar field). Verified NOT discoverable by either probe:
	// the single-variable presence probe DOES trip InvalidOIDCConfigError
	// when probing any one of the three alone (unlike the allowlist
	// group's own "all three empty" trigger, which no single-variable
	// probe can ever produce), but envVarNamesOf's own pass-2 "lax=false"
	// tier additionally requires the error text to contain "required" or
	// "missing" before it will extract ANY bare NARVI_ token from an
	// EnvVar-less error's message -- and this error's own wording
	// ("must be set together or all left empty... almost certainly a
	// misconfiguration") contains neither word, exactly like
	// InvalidObjectStoreCredentialsError's identical wording immediately
	// above. All three are added back by hand so the template stays
	// usable against a real OIDC provider, not just whatever the probes
	// happen to trigger with everything else left empty.
	"NARVI_OIDC_ISSUER",
	"NARVI_OIDC_CLIENT_ID",
	"NARVI_OIDC_CLIENT_SECRET",
}

// probeValuesByVar supplies a syntactically-valid placeholder for the
// small subset of NARVI_* variables whose own parsing would otherwise
// reject a bare, unstructured string -- an integer field, a boolean
// field, or a fixed enum -- so that RequiredConfigEnvVars' single-
// variable presence probe (below) doesn't trip that variable's OWN,
// unrelated validation error while probing whether its presence gates
// some OTHER variable. This is a nicety, never a correctness requirement:
// a probed variable that fails its own validation is explicitly allowed
// to (see RequiredConfigEnvVars' own doc comment) -- the probe loop
// never counts the probed variable itself as required from its own
// probe, so a self-inflicted validation error here is simply inert.
// A variable with a genuinely elaborate format (the PEM-encoded GitHub
// App private key, the exact-32-byte base64 token encryption key) is
// deliberately left off this list: nothing today gates any OTHER
// variable's requirement on either of those two parsing cleanly, so a
// laboriously-constructed valid instance of either would be dead weight.
var probeValuesByVar = map[string]string{
	"NARVI_DB_POOL_MAX_CONNS":                     "5",
	"NARVI_OBJECT_STORE_MAX_UPLOAD_BYTES":         "1",
	"NARVI_OBJECT_STORE_MAX_SESSION_UPLOAD_BYTES": "1",
	"NARVI_OBJECT_STORE_USE_PATH_STYLE":           "true",
	"NARVI_EPISTEMIC_CHECK_DEFAULT":               "true",
	"NARVI_SHADOW_MODE":                           "true",
	"NARVI_MCP_ENABLED":                           "true",
	"NARVI_LOG_LEVEL":                             "info",
	"NARVI_ROLLOUT_MODE":                          "open",
	"NARVI_INGRESS_ENABLED":                       "github,linear,slack",
	"NARVI_GITHUB_APP_ID":                         "123456",
}

// plausibleProbeValue returns probeValuesByVar's entry for name, or a
// generic non-empty placeholder for every variable that map doesn't
// special-case -- see probeValuesByVar's own doc comment for why a
// generic placeholder is a perfectly fine default. Never "": every
// single-variable presence probe needs this variable genuinely SET (the
// entire point of the probe), and the one existing presence-gate
// (objectStoreEndpointEnvVarName's `!= ""` check) needs nothing more
// specific than that.
func plausibleProbeValue(name string) string {
	if v, ok := probeValuesByVar[name]; ok {
		return v
	}
	return "probe-value"
}

// narviTokenPattern finds every bare NARVI_* token inside a string --
// either a rendered error message (envVarNamesOf's own fallback path,
// below) or a Go string literal's own decoded contents
// (narviStringLiteralsIn's own AST walk, below). One pattern, reused for
// both, since both are exactly the same kind of lookup: "does this text
// mention a NARVI_ name."
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
// shapes Load's own source used to say "this variable is missing." That
// scan was replaced with a version that calls platform.LoadWithLookup
// (internal/platform/config.go's own seam, added for exactly this) and
// asks a different question entirely: not "does config.go's SOURCE look
// like it requires this variable," but "does platform.Load ACTUALLY
// refuse to boot without it."
//
// # Two behavioral probes (§41.1 review round 3, finding R1)
//
// A single empty-environment probe (NARVI_STAGE set, nothing else) only
// ever finds variables Load requires UNCONDITIONALLY: a variable that is
// only required once an operator sets some OTHER variable first sits
// behind an `if` that is off by default, so a MissingRequiredEnvError
// inside it never even runs against a totally empty environment. This
// function therefore runs TWO passes, per requiredConfigStages value:
//
//  1. The empty-environment probe: NARVI_STAGE alone. Whatever
//     MissingRequiredEnvError/InvalidHMACSecretError-shaped (or otherwise
//     named-field) error this trips is unconditionally required. Because
//     the environment is otherwise completely empty, NO error Load can
//     return here is anything OTHER than "this variable must be
//     supplied" -- see envVarNamesOf's own doc comment for why this
//     probe's fallback tier is deliberately laxer than the single-
//     variable probe's own fallback below (§41.1 review round 3, finding
//     R3).
//  2. A single-variable presence probe: for every NARVI_* variable
//     config.go's own package reads at all (read, below), one probe with
//     ONLY that variable set (plus NARVI_STAGE) -- plausibleProbeValue's
//     own doc comment explains the value chosen. Any newly-appearing
//     required variable this trips (other than the probed variable
//     itself failing its OWN validation, which is explicitly allowed and
//     explicitly ignored -- see the loop below) is required GATED ON
//     THAT ONE VARIABLE'S PRESENCE: today, that's exactly
//     NARVI_OBJECT_STORE_REGION/BUCKET, gated on
//     NARVI_OBJECT_STORE_ENDPOINT alone.
//
// This is a single hop, deliberately: it does not simulate every
// COMBINATION of variables Load's own branches could take (that would be
// re-implementing Load's own control flow a second time -- CLAUDE.md's
// own "no I/O... every state transition" convention, applied to this
// scanner too). Two classes of requirement remain outside what EITHER
// probe can find, and stay hand-listed in deploySecretExtraVars instead
// (see that var's own doc comment for the two concrete examples that
// exist today):
//
//   - Gated on a SPECIFIC VALUE of another variable, not just its bare
//     presence (e.g. a hypothetical NARVI_SANDBOX_PROVIDER=kubernetes
//     unlocking a NARVI_K8S_* bundle -- setting NARVI_SANDBOX_PROVIDER to
//     plausibleProbeValue's own generic placeholder would very likely
//     fail ITS OWN validation before ever reaching that branch, so the
//     gate would never open during a probe).
//   - Gated on two-or-more variables' JOINT state (the allowlist group's
//     "all three empty together"; the object-store credential pair's
//     "must match, not merely both be present").
//
// # NARVI_STAGE itself (§41.1 review round 2, finding R2)
//
// Every probe above -- both passes -- presets NARVI_STAGE just to get
// past Load's own stage check, so NEITHER probe can ever see Load report
// it missing. required[platform.StageEnvVarName] is therefore set
// explicitly, once, after both probes run, rather than relying on either
// probe to find it.
//
// # read is a Go-syntax scan, not a raw-text one (§41.1 review round 3, finding R4)
//
// read is every NARVI_* token appearing inside a Go STRING LITERAL
// (go/ast's own *ast.BasicLit, kind token.STRING) anywhere in
// internal/platform's own non-test source files -- narviStringLiteralsIn,
// below. Deliberately AST-based, not a regexp over the file's raw bytes:
// go/parser never turns a comment into an *ast.BasicLit, so a NARVI_ name
// typed into a comment (documentation, a stale TODO, a copy-pasted
// example) can never inflate this set the way a plain text scan could --
// only a name Go's own compiler would treat as a real string value counts.
// TestDeploySecretTemplate's own "names a variable config.go no longer
// reads at all" direction only ever needs this superset (a literal env
// var name appearing in the platform package's source at all, in any
// shape whatsoever), never a precise shape-by-shape classification the
// way "required" needs.
func RequiredConfigEnvVars(configPath string) (required map[string]bool, read map[string]bool, err error) {
	read, err = narviStringLiteralsIn(filepath.Dir(configPath))
	if err != nil {
		return nil, nil, err
	}

	required = map[string]bool{}

	// Pass 1: the empty-environment probe -- see this function's own doc
	// comment, tier 1.
	for _, stage := range requiredConfigStages {
		env := map[string]string{platform.StageEnvVarName: string(stage)}
		if loadErr := probeLoad(env); loadErr != nil {
			for _, leaf := range flattenJoinedErrors(loadErr) {
				for _, name := range envVarNamesOf(leaf, true) {
					required[name] = true
				}
			}
		}
	}

	// Pass 2: one single-variable presence probe per (stage, variable
	// config.go reads) pair -- see this function's own doc comment, tier
	// 2.
	for _, stage := range requiredConfigStages {
		for v := range read {
			if v == platform.StageEnvVarName {
				// Already the fixed axis of every probe above and below
				// -- see this function's own "NARVI_STAGE itself"
				// section.
				continue
			}
			env := map[string]string{
				platform.StageEnvVarName: string(stage),
				v:                        plausibleProbeValue(v),
			}
			loadErr := probeLoad(env)
			if loadErr == nil {
				continue
			}
			for _, leaf := range flattenJoinedErrors(loadErr) {
				for _, name := range envVarNamesOf(leaf, false) {
					if name == v {
						// Never count the probed variable itself as
						// required from its own presence probe -- its
						// OWN validation is allowed to fail here (see
						// probeValuesByVar's own doc comment), and doing
						// so must never be mistaken for Load requiring
						// it. If the empty-environment probe (pass 1,
						// above) already found it required, it is
						// already in `required` regardless.
						continue
					}
					required[name] = true
				}
			}
		}
	}

	// NARVI_STAGE itself -- see this function's own doc comment,
	// "NARVI_STAGE itself" section (§41.1 review round 2, finding R2).
	required[platform.StageEnvVarName] = true

	return required, read, nil
}

// probeLoad runs platform.LoadWithLookup against exactly env (no fallback
// to the real process environment), returning whatever error it produces.
func probeLoad(env map[string]string) error {
	lookup := func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
	_, err := platform.LoadWithLookup(lookup)
	return err
}

// narviStringLiteralsIn parses every non-test .go file directly inside
// dir and returns the set of every NARVI_* token appearing inside a Go
// STRING literal anywhere in that package -- see RequiredConfigEnvVars'
// own doc comment, "read is a Go-syntax scan" section, for why this is
// an AST walk rather than a regexp over the raw file bytes.
func narviStringLiteralsIn(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("ops: read dir %s: %w", dir, err)
	}

	names := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil, fmt.Errorf("ops: parse %s: %w", path, parseErr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(lit.Value)
			if unquoteErr != nil {
				return true
			}
			for _, m := range narviTokenPattern.FindAllString(value, -1) {
				names[m] = true
			}
			return true
		})
	}
	return names, nil
}

// flattenJoinedErrors recursively expands every errors.Join tree (Load's
// own Unwrap() []error shape) into its leaf errors, so envVarNamesOf below
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
//     not a hard-coded list of type names: any error type platform.Load
//     returns that exposes a string field named EnvVar is picked up, with
//     nothing here needing to change the day Load starts returning a new
//     one. MissingRequiredEnvError, InvalidHMACSecretError, and
//     InvalidObjectStoreMaxBytesError all satisfy this today.
//
//  2. A fallback, for an error that carries NO structured field at all --
//     a plain fmt.Errorf, however it was built. Every such token in the
//     error's own rendered message is a candidate; whether that candidate
//     also needs the message to say "required" or "missing" depends on
//     lax:
//
//     lax == true (the empty-environment probe only -- §41.1 review round
//     3, finding R3): every NARVI_* token in the message counts, full
//     stop. Under a totally empty probe environment, Load's own source
//     (as of this writing) cannot produce ANY error that both (a) fires
//     with nothing set and (b) names a NARVI_ variable, unless that
//     variable must be supplied -- EmptyAllowlistError's own "at least
//     one of NARVI_ALLOWED_EMAIL_DOMAINS, ..., ... must be set" is exactly
//     this shape (it says neither "required" nor "missing"), and the old
//     wording filter dropped it silently even though, in this specific
//     empty-environment context, it unambiguously names variables an
//     operator must set.
//
//     lax == false (every single-variable presence probe): the message
//     must additionally contain "required" or "missing" before any token
//     is extracted. This probe's environment is NOT empty (the probed
//     variable itself is set, to plausibleProbeValue's own placeholder),
//     so an error here can ALSO be that variable's own unrelated parse
//     failure (e.g. InvalidObjectStoreCredentialsError's "must be set
//     together or both left empty," which names two OTHER variables that
//     are not actually required by this probe at all) -- the stricter
//     wording filter is this tier's guard against treating that kind of
//     incidental mention as a requirement.
func envVarNamesOf(err error, lax bool) []string {
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
	if !lax {
		lower := strings.ToLower(msg)
		if !strings.Contains(lower, "required") && !strings.Contains(lower, "missing") {
			return nil
		}
	}
	return narviTokenPattern.FindAllString(msg, -1)
}
