package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/credentials"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

func strPtrKC(s string) *string { return &s }

func TestApplyClusterBinding_Nil(t *testing.T) {
	dir := t.TempDir()
	env, state := applyClusterBinding(context.Background(), &fakeMinter{}, "sess-1", "tok", 1, nil, nil, fastMintTimeouts(), dir)
	if env != nil {
		t.Errorf("applyClusterBinding(nil, ...) env = %v, want nil", env)
	}
	if state != nil {
		t.Errorf("applyClusterBinding(nil, ...) state = %v, want nil", state)
	}
	if _, err := os.Stat(filepath.Join(dir, kubeconfigFileName)); err == nil {
		t.Errorf("kubeconfig file must not be written when cluster is nil")
	}
}

func TestApplyClusterBinding_Static(t *testing.T) {
	dir := t.TempDir()
	params, _ := json.Marshal(map[string]string{"secretName": "KUBE_STATIC_CONFIG"})
	cluster := &credentials.CloudIdentityConfigCluster{
		Name: "prod", AuthKind: "static", Params: params,
	}
	secrets := map[string]string{"KUBE_STATIC_CONFIG": "apiVersion: v1\nkind: Config\n# a real uploaded kubeconfig, verbatim\n"}

	env, state := applyClusterBinding(context.Background(), &fakeMinter{}, "sess-1", "tok", 1, cluster, secrets, fastMintTimeouts(), dir)
	wantPath := filepath.Join(dir, kubeconfigFileName)
	if len(env) != 1 || env[0] != "KUBECONFIG="+wantPath {
		t.Fatalf("env = %v, want [KUBECONFIG=%s]", env, wantPath)
	}
	if state != nil {
		t.Errorf("state = %+v, want nil -- the static rung mints nothing, there is no token to refresh", state)
	}

	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read kubeconfig: %v", err)
	}
	if string(got) != secrets["KUBE_STATIC_CONFIG"] {
		t.Errorf("kubeconfig content = %q, want the sandbox secret's own value VERBATIM (%q) -- §27.4: never env-var-expanded", got, secrets["KUBE_STATIC_CONFIG"])
	}
}

func TestApplyClusterBinding_StaticMissingSecret(t *testing.T) {
	dir := t.TempDir()
	params, _ := json.Marshal(map[string]string{"secretName": "KUBE_STATIC_CONFIG"})
	cluster := &credentials.CloudIdentityConfigCluster{Name: "prod", AuthKind: "static", Params: params}

	env, state := applyClusterBinding(context.Background(), &fakeMinter{}, "sess-1", "tok", 1, cluster, map[string]string{}, fastMintTimeouts(), dir)
	if env != nil || state != nil {
		t.Errorf("applyClusterBinding() = (%v, %v), want (nil, nil) (referenced secret did not resolve)", env, state)
	}
}

func TestApplyClusterBinding_Cloud(t *testing.T) {
	dir := t.TempDir()
	params, _ := json.Marshal(map[string]string{"cloud": "aws", "region": "us-east-1"})
	cluster := &credentials.CloudIdentityConfigCluster{
		Name: "prod-eks", AuthKind: "cloud", Params: params,
		ServerURL: strPtrKC("https://ABCDEF.gr7.us-east-1.eks.amazonaws.com"),
		CaBundle:  strPtrKC("-----BEGIN CERTIFICATE-----\nMIIB...\n-----END CERTIFICATE-----\n"),
	}

	env, state := applyClusterBinding(context.Background(), &fakeMinter{}, "sess-1", "tok", 1, cluster, nil, fastMintTimeouts(), dir)
	wantPath := filepath.Join(dir, kubeconfigFileName)
	if len(env) != 1 || env[0] != "KUBECONFIG="+wantPath {
		t.Fatalf("env = %v, want [KUBECONFIG=%s]", env, wantPath)
	}
	if state != nil {
		t.Errorf("state = %+v, want nil -- the cloud rung mints nothing of ITS OWN (it rides §27.3's already-minted cloud_identity_bindings tokens)", state)
	}

	doc := readKubeconfigDoc(t, wantPath)
	if len(doc.Clusters) != 1 || doc.Clusters[0].Cluster.Server != *cluster.ServerURL {
		t.Errorf("clusters = %+v, want server %q", doc.Clusters, *cluster.ServerURL)
	}
	wantCA := base64.StdEncoding.EncodeToString([]byte(*cluster.CaBundle))
	if doc.Clusters[0].Cluster.CertificateAuthorityData != wantCA {
		t.Errorf("certificate-authority-data = %q, want base64(caBundle) = %q", doc.Clusters[0].Cluster.CertificateAuthorityData, wantCA)
	}
	if len(doc.Users) != 1 {
		t.Fatalf("users = %+v, want exactly 1", doc.Users)
	}
	exec := doc.Users[0].User.Exec
	if exec == nil {
		t.Fatalf("exec = nil, want a populated exec stanza for the cloud rung")
	}
	if exec.Command != "aws" {
		t.Errorf("exec.command = %q, want %q", exec.Command, "aws")
	}
	wantArgs := []string{"eks", "get-token", "--cluster-name", "prod-eks", "--region", "us-east-1"}
	if strings.Join(exec.Args, ",") != strings.Join(wantArgs, ",") {
		t.Errorf("exec.args = %v, want %v", exec.Args, wantArgs)
	}
	if doc.Users[0].User.TokenFile != "" {
		t.Errorf("tokenFile = %q, want empty -- the cloud rung authenticates via exec, never tokenFile", doc.Users[0].User.TokenFile)
	}
}

func TestApplyClusterBinding_CloudGCPNoRegionArg(t *testing.T) {
	dir := t.TempDir()
	params, _ := json.Marshal(map[string]string{"cloud": "gcp"})
	cluster := &credentials.CloudIdentityConfigCluster{
		Name: "prod-gke", AuthKind: "cloud", Params: params,
		ServerURL: strPtrKC("https://gke.example"),
		CaBundle:  strPtrKC("ca-bundle"),
	}
	env, _ := applyClusterBinding(context.Background(), &fakeMinter{}, "sess-1", "tok", 1, cluster, nil, fastMintTimeouts(), dir)
	if len(env) != 1 {
		t.Fatalf("env = %v, want 1 entry", env)
	}
	doc := readKubeconfigDoc(t, filepath.Join(dir, kubeconfigFileName))
	exec := doc.Users[0].User.Exec
	if exec == nil {
		t.Fatalf("exec = nil, want a populated exec stanza")
	}
	if exec.Command != "gke-gcloud-auth-plugin" {
		t.Errorf("exec.command = %q, want %q", exec.Command, "gke-gcloud-auth-plugin")
	}
	if len(exec.Args) != 0 {
		t.Errorf("exec.args = %v, want empty", exec.Args)
	}
}

func TestApplyClusterBinding_CloudMissingServerURL(t *testing.T) {
	dir := t.TempDir()
	params, _ := json.Marshal(map[string]string{"cloud": "aws"})
	cluster := &credentials.CloudIdentityConfigCluster{Name: "prod", AuthKind: "cloud", Params: params, CaBundle: strPtrKC("ca")}
	env, state := applyClusterBinding(context.Background(), &fakeMinter{}, "sess-1", "tok", 1, cluster, nil, fastMintTimeouts(), dir)
	if env != nil || state != nil {
		t.Errorf("applyClusterBinding() = (%v, %v), want (nil, nil) (missing serverUrl)", env, state)
	}
}

// --- AuthKindOIDC: the fixed rung (adversarial-review HIGH fix) ---
//
// This rung previously rendered an exec-plugin kubeconfig invoking this
// SAME binary's own now-removed "kube-credential" subcommand -- see
// kubeconfig.go's own top doc comment ("Design correction") for why that
// design shipped structurally non-functional (every process that can
// ever run `kubectl` has NARVI_SESSION_CONFIG deliberately stripped from
// its own env, so the subcommand's re-exec of this binary had nothing to
// read that variable from). These tests pin the REPLACEMENT design: a
// minted token written to disk and referenced by the kubeconfig's own
// `tokenFile` field -- no subcommand, no exec plugin, no env var at all
// for the credential itself.

func TestApplyClusterBinding_OIDC(t *testing.T) {
	dir := t.TempDir()
	params, _ := json.Marshal(map[string]string{"clientId": "my-oidc-client"})
	cluster := &credentials.CloudIdentityConfigCluster{
		Name: "self-managed", AuthKind: "oidc", Params: params,
		ServerURL: strPtrKC("https://k8s.example:6443"),
		CaBundle:  strPtrKC("ca-bundle"),
	}
	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{
		"my-oidc-client": {Token: "minted-oidc-jwt", ExpiresAt: time.Now().Add(10 * time.Minute)},
	}}

	env, state := applyClusterBinding(context.Background(), m, "sess-1", "sandbox-token", 3, cluster, nil, fastMintTimeouts(), dir)
	wantPath := filepath.Join(dir, kubeconfigFileName)
	if len(env) != 1 || env[0] != "KUBECONFIG="+wantPath {
		t.Fatalf("env = %v, want [KUBECONFIG=%s]", env, wantPath)
	}
	if state == nil {
		t.Fatalf("state = nil, want a non-nil cloudIdentityBindingState -- the oidc rung's own minted token must be registered for half-life refresh")
	}
	if state.Audience != "my-oidc-client" {
		t.Errorf("state.Audience = %q, want %q", state.Audience, "my-oidc-client")
	}
	wantTokenPath := filepath.Join(dir, oidcTokenFileName)
	if state.TokenPath != wantTokenPath {
		t.Errorf("state.TokenPath = %q, want %q", state.TokenPath, wantTokenPath)
	}

	doc := readKubeconfigDoc(t, wantPath)
	if len(doc.Users) != 1 {
		t.Fatalf("users = %+v, want exactly 1", doc.Users)
	}
	if doc.Users[0].User.Exec != nil {
		t.Errorf("exec = %+v, want nil -- the oidc rung must never render an exec stanza (that design was removed)", doc.Users[0].User.Exec)
	}
	if doc.Users[0].User.TokenFile != wantTokenPath {
		t.Errorf("tokenFile = %q, want %q", doc.Users[0].User.TokenFile, wantTokenPath)
	}

	gotToken, err := os.ReadFile(wantTokenPath)
	if err != nil {
		t.Fatalf("read oidc token file: %v", err)
	}
	if string(gotToken) != "minted-oidc-jwt" {
		t.Errorf("token file content = %q, want %q", gotToken, "minted-oidc-jwt")
	}
	fi, err := os.Stat(wantTokenPath)
	if err != nil {
		t.Fatalf("stat oidc token file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("oidc token file mode = %o, want 0600", perm)
	}
}

// TestApplyClusterBinding_OIDC_TokenFilePathMatchesRefreshState is this
// fix's own MUTATION-TEST target (report this name): the path the
// rendered kubeconfig names in its own `tokenFile` field MUST be
// byte-identical to state.TokenPath -- the path
// runCloudIdentityRefreshLoop actually keeps refreshed (main.go folds
// this returned state into the SAME cloudIdentityStates slice as every
// §27.3 cloud_identity_bindings entry). If a future edit changes
// oidcTokenFileName in one of the two call sites but not the other (or
// wires the wrong path into either renderTokenFileKubeconfig or the
// returned state), kubectl would silently authenticate against a file
// nobody is refreshing -- exactly the "kubectl breaks partway into a
// session" failure mode this Step's own brief warned against. This test
// makes that divergence fail loudly instead.
func TestApplyClusterBinding_OIDC_TokenFilePathMatchesRefreshState(t *testing.T) {
	dir := t.TempDir()
	params, _ := json.Marshal(map[string]string{"clientId": "client-x"})
	cluster := &credentials.CloudIdentityConfigCluster{
		Name: "self-managed", AuthKind: "oidc", Params: params,
		ServerURL: strPtrKC("https://k8s.example:6443"),
		CaBundle:  strPtrKC("ca-bundle"),
	}
	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{"client-x": {Token: "tok"}}}

	env, state := applyClusterBinding(context.Background(), m, "sess-1", "tok", 1, cluster, nil, fastMintTimeouts(), dir)
	if len(env) != 1 || state == nil {
		t.Fatalf("applyClusterBinding() = (%v, %v), want a populated env and non-nil state", env, state)
	}

	doc := readKubeconfigDoc(t, filepath.Join(dir, kubeconfigFileName))
	if doc.Users[0].User.TokenFile != state.TokenPath {
		t.Fatalf("rendered kubeconfig's own tokenFile = %q, want it to EXACTLY match the refreshed state's own TokenPath = %q -- these must never diverge, or kubectl authenticates against a file nothing refreshes", doc.Users[0].User.TokenFile, state.TokenPath)
	}
}

func TestApplyClusterBinding_OIDC_MintFailure(t *testing.T) {
	dir := t.TempDir()
	params, _ := json.Marshal(map[string]string{"clientId": "my-oidc-client"})
	cluster := &credentials.CloudIdentityConfigCluster{
		Name: "self-managed", AuthKind: "oidc", Params: params,
		ServerURL: strPtrKC("https://k8s.example:6443"),
		CaBundle:  strPtrKC("ca-bundle"),
	}
	m := &fakeMinter{failFirstN: 100, statusCode: http.StatusForbidden}

	env, state := applyClusterBinding(context.Background(), m, "sess-1", "tok", 1, cluster, nil, fastMintTimeouts(), dir)
	if env != nil || state != nil {
		t.Errorf("applyClusterBinding() = (%v, %v), want (nil, nil) -- a failed mint must skip kubeconfig injection entirely", env, state)
	}
	if _, err := os.Stat(filepath.Join(dir, kubeconfigFileName)); err == nil {
		t.Errorf("kubeconfig file must not be written when the oidc rung's own token mint fails")
	}
}

func TestApplyClusterBinding_OIDCMissingServerURL(t *testing.T) {
	dir := t.TempDir()
	params, _ := json.Marshal(map[string]string{"clientId": "c"})
	cluster := &credentials.CloudIdentityConfigCluster{Name: "x", AuthKind: "oidc", Params: params, CaBundle: strPtrKC("ca")}
	env, state := applyClusterBinding(context.Background(), &fakeMinter{}, "sess-1", "tok", 1, cluster, nil, fastMintTimeouts(), dir)
	if env != nil || state != nil {
		t.Errorf("applyClusterBinding() = (%v, %v), want (nil, nil) (missing serverUrl)", env, state)
	}
}

func TestApplyClusterBinding_UnrecognizedAuthKind(t *testing.T) {
	dir := t.TempDir()
	cluster := &credentials.CloudIdentityConfigCluster{Name: "x", AuthKind: "manual", Params: json.RawMessage(`{}`)}
	env, state := applyClusterBinding(context.Background(), &fakeMinter{}, "sess-1", "tok", 1, cluster, nil, fastMintTimeouts(), dir)
	if env != nil || state != nil {
		t.Errorf("applyClusterBinding() = (%v, %v), want (nil, nil)", env, state)
	}
}

func TestApplyClusterBinding_InvalidParams(t *testing.T) {
	dir := t.TempDir()
	cluster := &credentials.CloudIdentityConfigCluster{
		Name: "x", AuthKind: "cloud", Params: json.RawMessage(`{}`), // missing "cloud" field
		ServerURL: strPtrKC("https://x"), CaBundle: strPtrKC("ca"),
	}
	env, state := applyClusterBinding(context.Background(), &fakeMinter{}, "sess-1", "tok", 1, cluster, nil, fastMintTimeouts(), dir)
	if env != nil || state != nil {
		t.Errorf("applyClusterBinding() = (%v, %v), want (nil, nil)", env, state)
	}
}

// --- kubeconfig file permissions ---

func TestApplyClusterBinding_KubeconfigPermissions(t *testing.T) {
	dir := t.TempDir()
	params, _ := json.Marshal(map[string]string{"secretName": "S"})
	cluster := &credentials.CloudIdentityConfigCluster{Name: "x", AuthKind: "static", Params: params}
	applyClusterBinding(context.Background(), &fakeMinter{}, "sess-1", "tok", 1, cluster, map[string]string{"S": "kubeconfig content"}, fastMintTimeouts(), dir)

	fi, err := os.Stat(filepath.Join(dir, kubeconfigFileName))
	if err != nil {
		t.Fatalf("stat kubeconfig: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("kubeconfig file mode = %o, want 0600", perm)
	}
}

// --- real-process proof: the oidc rung's own token reaches a REAL
// spawned hook through the existing hook path, with NO NARVI_SESSION_CONFIG
// anywhere in its environment ---

// TestOIDCClusterBindingTokenReachesRealSpawnedHook is this fix's own
// end-to-end proof, mirroring cloudidentity_test.go's own
// TestCloudIdentityTokenReachesRealSpawnedHook precedent exactly (same
// rationale: "a REAL process... via boot.RunHooks -- the SAME threaded-
// env seam every OTHER §27.1/§27.3 injected value already goes
// through"). This is also a mutation-test target in its own right: a
// spawned setup.sh -- with NARVI_SESSION_CONFIG deliberately absent from
// its env, exactly like a real kubectl invocation inside opencode's own
// stripped process tree -- reads ONLY $KUBECONFIG (the env entry
// applyClusterBinding actually returns), parses out the rendered
// kubeconfig's own `tokenFile:` line, and cats THAT file -- proving a
// process in the exact environment a real `kubectl` runs in can retrieve
// a valid credential at the exact path the rendered kubeconfig names,
// with no subcommand, no exec plugin, and no re-exposure of the
// sandbox's own bearer token anywhere in that process tree.
//
// On the 5-second hookTimeout below: see
// TestCloudIdentityTokenReachesRealSpawnedHook's own doc comment
// (cloudidentity_test.go) for the full measurement this test shares --
// both failed together under the same loaded `make test` run, and both
// were re-measured together under two independent full-suite runs (0.33s
// actual here, ~15x under budget). Left unchanged for the same reason.
func TestOIDCClusterBindingTokenReachesRealSpawnedHook(t *testing.T) {
	dir := t.TempDir()
	params, _ := json.Marshal(map[string]string{"clientId": "real-spawn-client"})
	cluster := &credentials.CloudIdentityConfigCluster{
		Name: "self-managed", AuthKind: "oidc", Params: params,
		ServerURL: strPtrKC("https://k8s.example:6443"),
		CaBundle:  strPtrKC("ca-bundle"),
	}
	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{
		"real-spawn-client": {Token: "real-spawned-process-kube-jwt"},
	}}

	env, state := applyClusterBinding(context.Background(), m, "sess-1", "tok", 1, cluster, nil, fastMintTimeouts(), dir)
	if len(env) != 1 || state == nil {
		t.Fatalf("applyClusterBinding() = (%v, %v), want a populated env and non-nil state", env, state)
	}

	workspaceDir := t.TempDir()
	probeFile := filepath.Join(t.TempDir(), "probe")
	// A real kubectl reads $KUBECONFIG, finds the `tokenFile:` line inside
	// it, and reads THAT file directly -- this script does the exact same
	// two-hop lookup using nothing but the one env var boot threads
	// through, proving the credential is reachable without
	// NARVI_SESSION_CONFIG (already absent -- boot.RunHooks strips it via
	// supervisor.EnvWithout, the SAME call every OTHER hook/opencode spawn
	// in this binary already makes).
	writeCloudIdentityTestScript(t, filepath.Join(workspaceDir, "repo-a", "setup.sh"),
		`token_file=$(grep 'tokenFile:' "$KUBECONFIG" | awk '{print $2}'); cat "$token_file" > `+probeFile)

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo-a", Primary: true}}

	err := boot.RunHooks(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeBuild, nil, nil, env,
		func(_, _, _ string, _, _ bool, _ float64) {}, 5*time.Second, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("RunHooks() error = %v, want nil", err)
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	if string(got) != "real-spawned-process-kube-jwt" {
		t.Errorf("token content as read by the REAL spawned setup.sh (via $KUBECONFIG's own tokenFile) = %q, want %q", got, "real-spawned-process-kube-jwt")
	}
}

// --- test helpers ---

func readKubeconfigDoc(t *testing.T, path string) kubeconfigDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read kubeconfig: %v", err)
	}
	var doc kubeconfigDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("kubeconfig is not valid YAML: %v", err)
	}
	return doc
}
