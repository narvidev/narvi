//go:build integration

// Integration test proving the objstore adapter actually round-trips
// against a real S3-compatible backend (a testcontainer running Versity
// S3 Gateway, "versitygw" -- see s3GatewayImage's own doc comment for why
// this replaced MinIO), not just against the httptest.Server stand-ins
// store_test.go/presign_test.go use for the unit-level HTTP-status
// classification table. Gated behind the "integration" build tag (needs
// Docker) so it does not run as part of the fast `make test` -- run via
// `make test-integration`. Mirrors internal/adapters/outbound/postgres/
// postgres_integration_test.go's own build-tag comment, package-naming
// (_test external package), and testcontainers-go conventions (§28.7:
// "the adapter's integration tests run against an S3-compatible
// testcontainer, the postgres:17-alpine testcontainers precedent").
package objstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/narvidev/narvi/internal/adapters/outbound/objstore"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// s3GatewayImage names versity/versitygw (Apache-2.0, github.com/versity/
// versitygw), a purpose-built S3-protocol gateway, pinned by BOTH an
// explicit release tag and its content digest (never ":latest" -- a
// local `docker compose pull` or a fresh testcontainers pull must not be
// able to silently change what this test runs against).
//
// This replaced MinIO (quay.io/minio/minio and Docker Hub's minio/minio)
// after BOTH registries independently started answering every anonymous
// pull with "unauthorized"/"pull access denied ... repository does not
// exist or may require 'docker login'" -- reproduced directly against
// this exact pinned tag from a developer machine and confirmed as the
// cause of CI's test-integration shard going red on every PR, not a rate
// limit (which would say so) and not transient (it reproduces every
// time).
//
// Four MinIO-replacement candidates were pulled and run for real,
// anonymously, against this exact test's own operations (PresignPut -> a
// real PUT, Stat, PresignGet with response-content-disposition -> a real
// GET, Delete, idempotent re-Delete) using the actual objstore.Store
// production code (github.com/aws/aws-sdk-go-v2/service/s3), not a
// separate hand-rolled signer:
//
//   - rustfs/rustfs: Apache-2.0, pullable anonymously, passed every
//     check via aws-sdk-go-v2 and correctly rejects a wrong-secret
//     presigned PUT with 403. Ruled out anyway: as of this writing its
//     own published tags are still pre-1.0 (alpha/beta/rc), a materially
//     less stable pin for a dependency CI relies on than the other two
//     candidates' tagged 1.x/4.x releases.
//   - chrislusf/seaweedfs (server -s3): Apache-2.0, pullable anonymously,
//     passed every check, correctly rejects a wrong-secret presigned PUT
//     with 403, and is the most battle-tested/widely-deployed of the
//     four. Ruled out for THIS narrow use only: its all-in-one
//     "server -s3" command boots an entire master+volume+filer+S3-gateway
//     cluster (plus unrelated Iceberg/Lance namespace servers neither
//     this adapter nor any Narvi code touches) and needs a mounted JSON
//     identity config file just to accept SigV4 credentials --
//     empirically ~3.4s to become ready here, roughly 8x versitygw's own
//     ~0.4s, for capability this test and docker-compose.dev.yml's dev
//     loop never use.
//   - adobe/s3mock: Apache-2.0, pullable anonymously, passed every
//     *functional* check -- but empirically does NOT validate SigV4 at
//     all: a presigned PUT signed with a deliberately wrong secret key
//     was accepted with 200 instead of rejected with 403/401. A backend
//     that accepts an incorrectly-signed write is not exercising the
//     real behavior this adapter's presigning exists to enforce, so it
//     was ruled out regardless of how convenient it otherwise is as a
//     pure unit-test double.
//   - versity/versitygw (chosen): Apache-2.0, pullable anonymously,
//     passed every check, correctly rejects a wrong-secret presigned PUT
//     with 403, is a stable tagged release (v1.8.0, not a pre-1.0 tag),
//     is purpose-built as a standalone S3 protocol gateway (its own
//     posix backend needs nothing but a filesystem directory --
//     no separate identity config file), has the smallest image of the
//     four (~31MB vs. rustfs's ~110MB and seaweedfs's ~92MB), and was
//     empirically the fastest to become ready (~0.4s, tied with rustfs,
//     both far ahead of seaweedfs).
const s3GatewayImage = "versity/versitygw:v1.8.0@sha256:30292fc2eeacc67a36993b01f7a7a5e3361a19cced0e80c1d71cfa2a4b0a2499"

// s3GatewayAccessKey/s3GatewaySecretKey are fixed, test-only SigV4
// credentials passed to the container via ROOT_ACCESS_KEY/ROOT_SECRET_KEY
// (versitygw's own root-account env vars, verified directly against this
// exact pinned image) -- never real credentials, matching
// docker-compose.dev.yml's own equally fixed dev-only pair.
const (
	s3GatewayAccessKey = "narvi-objstore-test"
	s3GatewaySecretKey = "narvi-objstore-test-secret"
)

// s3TestBucket is created fresh inside TestStore_S3RoundTrip via a raw
// admin *s3.Client (see that test) -- ports.BlobStore itself deliberately
// has no bucket-management method (§28.1: "one configured bucket per
// deployment", provisioned out of band, never by the adapter).
const s3TestBucket = "objstore-integration-test"

// startS3GatewayContainer starts a versitygw testcontainer bounded by a
// context.WithTimeout, deliberately WITHOUT the heavier errgroup+
// independent-watchdog race postgres_integration_test.go's own
// newMigrate/TestSchemaSqlcStoresPipeline uses around tcpostgres.Run.
//
// That heavier pattern exists there because of THREE separately observed,
// real CI hangs (see that file's own doc comment: CI runs 30831633470,
// 30834918806, 30838285218) inside testcontainers-go's own Docker-daemon-
// facing machinery, where even a per-call context.WithTimeout was
// empirically shown NOT to always cut the call off -- root-caused there to
// HOST-LEVEL contention (many packages' own containers starting
// concurrently under `go test ./...`), not to anything specific to
// Postgres. This package has no equivalent history of an observed hang,
// and go test-integration's own -p 1 (serialized package test binaries,
// see the Makefile's own comment on that target) already addresses the
// SAME host-contention root cause package-wide -- so a plain,
// context-bounded call is used here rather than pre-emptively copying the
// heavier construct onto a module with no demonstrated need for it. This
// is a judgment call, not a certainty: if this package ever shows the
// same hang symptom in real CI, promote it to the same errgroup+watchdog
// shape postgres_integration_test.go already uses (still via
// errgroup.Group.Go, never a naked `go` statement, either way -- §11).
//
// There is no dedicated testcontainers-go module for versitygw (unlike
// MinIO's own tcminio), so this uses testcontainers.GenericContainer
// directly. The entrypoint is overridden to `mkdir -p /data` before
// exec-ing the real binary: the pinned image's posix backend refuses to
// start against a top-level directory that does not already exist
// (verified directly -- "chdir /data: no such file or directory" against
// this exact image when /data is not pre-created), and no volume is
// mounted here since the container is single-use and torn down at the
// end of the test.
func startS3GatewayContainer(t *testing.T, ctx context.Context) testcontainers.Container {
	t.Helper()

	startCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:      s3GatewayImage,
			Entrypoint: []string{"sh", "-c"},
			Cmd:        []string{"mkdir -p /data && exec /usr/local/bin/versitygw posix /data"},
			Env: map[string]string{
				"ROOT_ACCESS_KEY": s3GatewayAccessKey,
				"ROOT_SECRET_KEY": s3GatewaySecretKey,
			},
			ExposedPorts: []string{"7070/tcp"},
			WaitingFor: wait.ForAll(
				wait.ForListeningPort("7070/tcp"),
				wait.ForLog("VersityGW"),
			),
		},
		Started: true,
	}

	container, err := testcontainers.GenericContainer(startCtx, req)
	if err != nil {
		t.Fatalf("start versitygw container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminate container: %v", err)
		}
	})
	return container
}

// adminClient builds a raw *s3.Client (bypassing objstore.Store entirely)
// used ONLY to create the test bucket before constructing the Store under
// test -- BlobStore has no bucket-management method by design (§28.1), so
// the test has to reach for the SDK directly here, exactly as a real
// deployment's own out-of-band provisioning step would.
func adminClient(t *testing.T, endpoint, username, password string) *s3.Client {
	t.Helper()
	return s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(username, password, ""),
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
	})
}

// TestStore_S3RoundTrip exercises the full ports.BlobStore contract
// against a real backend: PresignPut -> an actual PUT via a plain
// http.Client -> Stat returns the correct SizeBytes/ETag -> PresignGet ->
// an actual GET round-trips the same bytes -> Delete -> Stat now returns
// ports.ErrBlobNotFound -> Delete again on the now-absent key still
// returns nil (idempotency, asserted explicitly, not just assumed).
//
// Named "S3RoundTrip", not "MinIORoundTrip" -- nothing else in the repo
// pinned the old name (verified: grep -rn "TestStore_MinIORoundTrip"
// found only this file), and the backend under test is no longer MinIO
// (see s3GatewayImage's own doc comment).
func TestStore_S3RoundTrip(t *testing.T) {
	ctx := context.Background()

	container := startS3GatewayContainer(t, ctx)

	endpoint, err := container.PortEndpoint(ctx, "7070/tcp", "http")
	if err != nil {
		t.Fatalf("PortEndpoint: %v", err)
	}

	admin := adminClient(t, endpoint, s3GatewayAccessKey, s3GatewaySecretKey)
	if _, err := admin.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s3TestBucket)}); err != nil {
		t.Fatalf("CreateBucket(%q): %v", s3TestBucket, err)
	}

	timeouts := platform.DefaultTimeouts()
	timeouts.ObjectStoreHTTPClientTimeout = 30 * time.Second

	store, err := objstore.New(objstore.Config{
		Endpoint:        endpoint,
		Region:          "us-east-1", // versitygw accepts any string (§28.7), same as MinIO did.
		Bucket:          s3TestBucket,
		AccessKeyID:     s3GatewayAccessKey,
		SecretAccessKey: s3GatewaySecretKey,
		UsePathStyle:    true, // required for MinIO/versitygw-style backends.
		Timeouts:        timeouts,
	})
	if err != nil {
		t.Fatalf("objstore.New: %v", err)
	}

	const key = ports.BlobKey("sessions/integration-test-session/uploads/integration-test-upload")
	content := []byte("hello from the objstore S3 integration test, round-tripped byte for byte")

	// -- PresignPut, then a real PUT via a plain http.Client. --
	putSpec := ports.PresignPutSpec{
		Key:           key,
		ContentType:   "text/plain; charset=utf-8",
		ContentLength: int64(len(content)),
		TTL:           time.Minute,
	}
	putURL, err := store.PresignPut(ctx, putSpec)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}

	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL.URL, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("build PUT request: %v", err)
	}
	for k, v := range putURL.Headers {
		putReq.Header.Set(k, v)
	}
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT via presigned URL: %v", err)
	}
	putBody, _ := io.ReadAll(putResp.Body)
	_ = putResp.Body.Close()
	if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
		t.Fatalf("PUT via presigned URL: status = %d, body = %s", putResp.StatusCode, putBody)
	}

	// -- Stat: correct SizeBytes/ETag. --
	info, err := store.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.SizeBytes != int64(len(content)) {
		t.Errorf("Stat().SizeBytes = %d, want %d", info.SizeBytes, len(content))
	}
	if info.ETag == "" {
		t.Error("Stat().ETag is empty, want a real ETag")
	}
	if len(info.ETag) >= 2 && (info.ETag[0] == '"' || info.ETag[len(info.ETag)-1] == '"') {
		t.Errorf("Stat().ETag = %q, want surrounding quotes already trimmed", info.ETag)
	}

	// -- PresignGet, then a real GET, confirm byte-for-byte round trip. --
	const wantFilename = `report "final".txt`
	getURL, err := store.PresignGet(ctx, ports.PresignGetSpec{
		Key:              key,
		TTL:              time.Minute,
		ResponseFilename: wantFilename,
	})
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	getResp, err := http.Get(getURL.URL) //nolint:gosec // getURL.URL is our own freshly-minted presigned URL, not user input.
	if err != nil {
		t.Fatalf("GET via presigned URL: %v", err)
	}
	gotBytes, err := io.ReadAll(getResp.Body)
	_ = getResp.Body.Close()
	if err != nil {
		t.Fatalf("read GET body: %v", err)
	}
	if getResp.StatusCode < 200 || getResp.StatusCode >= 300 {
		t.Fatalf("GET via presigned URL: status = %d, body = %s", getResp.StatusCode, gotBytes)
	}
	if !bytes.Equal(gotBytes, content) {
		t.Errorf("GET body = %q, want %q (byte-for-byte round trip)", gotBytes, content)
	}
	// Tightened assertion (review-fix coverage addition, FIX J): this
	// test deliberately picks a hostile, quote-bearing wantFilename above
	// specifically to exercise presign.go's own mime.FormatMediaType
	// escaping against a REAL backend -- asserting only "non-empty" left
	// that escaping entirely unverified (wantFilename was write-only).
	// Decode the REAL header the same way any real HTTP client would
	// (mime.ParseMediaType), rather than string-matching the raw escaped
	// form, which would be brittle against Go's own internal choice of
	// backslash-escaping vs RFC 2231 percent-encoding.
	gotDisposition := getResp.Header.Get("Content-Disposition")
	if gotDisposition == "" {
		t.Fatal("GET response Content-Disposition header is empty, want the forced-download filename")
	}
	dispositionType, params, err := mime.ParseMediaType(gotDisposition)
	if err != nil {
		t.Fatalf("parse Content-Disposition %q: %v", gotDisposition, err)
	}
	if dispositionType != "attachment" {
		t.Errorf("Content-Disposition type = %q, want %q (§28.5: user-supplied content must never render inline)", dispositionType, "attachment")
	}
	if params["filename"] != wantFilename {
		t.Errorf("Content-Disposition filename = %q, want %q (byte-for-byte round trip of the hostile, quote-bearing filename through the real backend)", params["filename"], wantFilename)
	}

	// -- Delete, then Stat now reports ErrBlobNotFound. --
	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Stat(ctx, key); !errors.Is(err, ports.ErrBlobNotFound) {
		t.Errorf("Stat() after Delete: err = %v, want errors.Is(err, ports.ErrBlobNotFound)", err)
	}

	// -- Delete again on the now-absent key: still idempotent (nil), not
	// re-surfaced as an error. --
	if err := store.Delete(ctx, key); err != nil {
		t.Errorf("second Delete() on an already-absent key: err = %v, want nil (idempotent)", err)
	}
}
