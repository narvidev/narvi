//go:build integration

// Integration test proving the objstore adapter actually round-trips
// against a real S3-compatible backend (a MinIO testcontainer), not just
// against the httptest.Server stand-ins store_test.go/presign_test.go use
// for the unit-level HTTP-status classification table. Gated behind the
// "integration" build tag (needs Docker) so it does not run as part of
// the fast `make test` -- run via `make test-integration`. Mirrors
// internal/adapters/outbound/postgres/postgres_integration_test.go's own
// build-tag comment, package-naming (_test external package), and
// testcontainers-go conventions (§28.7: "the adapter's integration tests
// run against a MinIO testcontainer, the postgres:17-alpine testcontainers
// precedent").
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
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/narvidev/narvi/internal/adapters/outbound/objstore"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// minioImage names quay.io, MinIO's own registry, NOT Docker Hub.
//
// The tag is unchanged and was always correct; what moved is where it can
// be fetched from. `minio/minio` on Docker Hub now answers every anonymous
// pull with "pull access denied ... repository does not exist or may
// require 'docker login'" -- not a rate limit, which would say so, and not
// transient: it reproduces from a developer machine as readily as from CI.
// This service's own compose comment had already recorded that Docker Hub
// publishing "appears to have gone quiet after this exact release"; the
// repository has since stopped answering altogether.
//
// quay.io/minio/minio serves the SAME pinned tag (verified by pulling it),
// so nothing about what this test runs against changes. Still deliberately
// not ":latest" -- see docker-compose.dev.yml's own minio comment, which
// names the same registry for the same reason.
const minioImage = "quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z"

// minioTestBucket is created fresh inside TestStore_MinIORoundTrip via a
// raw admin *s3.Client (see that test) -- ports.BlobStore itself
// deliberately has no bucket-management method (§28.1: "one configured
// bucket per deployment", provisioned out of band, never by the adapter).
const minioTestBucket = "objstore-integration-test"

// startMinIOContainer starts a MinIO testcontainer bounded by a
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
func startMinIOContainer(t *testing.T, ctx context.Context) *tcminio.MinioContainer {
	t.Helper()

	startCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	container, err := tcminio.Run(startCtx, minioImage)
	if err != nil {
		t.Fatalf("start minio container: %v", err)
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

// TestStore_MinIORoundTrip exercises the full ports.BlobStore contract
// against a real backend: PresignPut -> an actual PUT via a plain
// http.Client -> Stat returns the correct SizeBytes/ETag -> PresignGet ->
// an actual GET round-trips the same bytes -> Delete -> Stat now returns
// ports.ErrBlobNotFound -> Delete again on the now-absent key still
// returns nil (idempotency, asserted explicitly, not just assumed).
func TestStore_MinIORoundTrip(t *testing.T) {
	ctx := context.Background()

	container := startMinIOContainer(t, ctx)

	endpoint, err := container.PortEndpoint(ctx, "9000/tcp", "http")
	if err != nil {
		t.Fatalf("PortEndpoint: %v", err)
	}

	admin := adminClient(t, endpoint, container.Username, container.Password)
	if _, err := admin.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(minioTestBucket)}); err != nil {
		t.Fatalf("CreateBucket(%q): %v", minioTestBucket, err)
	}

	timeouts := platform.DefaultTimeouts()
	timeouts.ObjectStoreHTTPClientTimeout = 30 * time.Second

	store, err := objstore.New(objstore.Config{
		Endpoint:        endpoint,
		Region:          "us-east-1", // MinIO accepts any string (§28.7).
		Bucket:          minioTestBucket,
		AccessKeyID:     container.Username,
		SecretAccessKey: container.Password,
		UsePathStyle:    true, // required for MinIO-style backends.
		Timeouts:        timeouts,
	})
	if err != nil {
		t.Fatalf("objstore.New: %v", err)
	}

	const key = ports.BlobKey("sessions/integration-test-session/uploads/integration-test-upload")
	content := []byte("hello from the objstore MinIO integration test, round-tripped byte for byte")

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
