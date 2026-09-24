//go:build integration

// Integration tests for §8.6's ("uploads, blob storage & the
// in-sandbox download_file tool", §28) mint/confirm/content endpoints,
// both auth variants, against a real Postgres instance -- gated behind
// the "integration" build tag, using this package's own shared rig
// (httpapi_integration_test.go). Run via `make test-integration`.
//
// These tests exercise the UPLOAD LIFECYCLE's own logic (mint's size/quota
// check, confirm's Stat-based verification + re-checked quota + guarded
// transition + event/outbox, content's presigned-redirect) against a
// fake, in-memory ports.BlobStore (fakeBlobStore, below) rather than a
// real S3-compatible backend -- real SigV4/HTTP-status-classification
// behavior is covered separately, exhaustively, by
// internal/adapters/outbound/objstore's own unit and S3-compatible-
// testcontainer integration tests (§28.7).
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
)

// --- fakeBlobStore: an in-memory, httptest.Server-backed ports.BlobStore ---

// fakeBlobStore is this package's own test-only ports.BlobStore: PresignPut/
// PresignGet return REAL URLs against a real httptest.Server, so a test's
// own http.Client can actually PUT/GET bytes exactly like a real
// sandbox/browser would follow the mint response and the content
// redirect -- Stat/Delete read/write the SAME in-memory map that server
// handler populates, so a size mismatch or a never-transferred object is
// observed exactly like it would be against a real backend.
type fakeBlobStore struct {
	mu      sync.Mutex
	objects map[ports.BlobKey][]byte
	server  *httptest.Server

	// statErrOverride, when non-nil, is returned by the NEXT Stat call
	// instead of the real in-memory lookup, then cleared (one-shot) --
	// lets a test force ErrBlobNotFound or a transient/permanent
	// BlobStoreError without needing a real network condition.
	statErrOverride error
}

func newFakeBlobStore(t *testing.T) *fakeBlobStore {
	t.Helper()
	f := &fakeBlobStore{objects: make(map[ports.BlobKey][]byte)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := ports.BlobKey(strings.TrimPrefix(r.URL.Path, "/"))
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			f.mu.Lock()
			f.objects[key] = body
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			f.mu.Lock()
			body, ok := f.objects[key]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if cd := r.URL.Query().Get("response-content-disposition"); cd != "" {
				w.Header().Set("Content-Disposition", cd)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeBlobStore) PresignPut(_ context.Context, spec ports.PresignPutSpec) (ports.PresignedURL, error) {
	return ports.PresignedURL{
		URL:       f.server.URL + "/" + string(spec.Key),
		ExpiresAt: time.Now().Add(spec.TTL),
		Headers:   map[string]string{},
	}, nil
}

func (f *fakeBlobStore) PresignGet(_ context.Context, spec ports.PresignGetSpec) (ports.PresignedURL, error) {
	target := f.server.URL + "/" + string(spec.Key)
	if spec.ResponseFilename != "" {
		target += "?response-content-disposition=" + url.QueryEscape(`attachment; filename="`+spec.ResponseFilename+`"`)
	}
	return ports.PresignedURL{URL: target, ExpiresAt: time.Now().Add(spec.TTL)}, nil
}

func (f *fakeBlobStore) Stat(_ context.Context, key ports.BlobKey) (ports.BlobInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statErrOverride != nil {
		err := f.statErrOverride
		f.statErrOverride = nil
		return ports.BlobInfo{}, err
	}
	body, ok := f.objects[key]
	if !ok {
		return ports.BlobInfo{}, ports.ErrBlobNotFound
	}
	return ports.BlobInfo{SizeBytes: int64(len(body)), ETag: "fake-etag"}, nil
}

func (f *fakeBlobStore) Delete(_ context.Context, key ports.BlobKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

// put writes body directly into f's in-memory map for key -- a test-only
// shortcut for seeding a row's own object bypassing a real PUT round trip
// (used when the object's PRESENCE is what's under test, not the PUT
// mechanics themselves, which the happy-path tests below already cover).
func (f *fakeBlobStore) put(key ports.BlobKey, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = body
}

var _ ports.BlobStore = (*fakeBlobStore)(nil)

// recordingBroadcaster is this package's own test-only ports.EventBroadcaster
// -- mirrors internal/app/uploadsweep/sweep_integration_test.go's own
// identical fake shape.
type recordingBroadcaster struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingBroadcaster) Broadcast(sessionID string, payload json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, sessionID+":"+string(payload))
}

func (r *recordingBroadcaster) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

var _ ports.EventBroadcaster = (*recordingBroadcaster)(nil)

// --- sandbox-bearer wire shapes (mirrors uploadmint.go/uploadconfirm.go's
// own unexported uploadMintResponse/uploadConfirmResponse -- this test
// package cannot import those directly, so it declares the identical JSON
// shape locally, exactly like postScmCredentialsFull's own precedent). ---

type sandboxMintResponse struct {
	UploadID  string            `json:"uploadId"`
	PutURL    string            `json:"putUrl"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt string            `json:"expiresAt"`
}

type sandboxConfirmResponse struct {
	Status        string  `json:"status"`
	FailureReason *string `json:"failureReason"`
}

// postSandboxUpload POSTs body to r.server.URL+path with the sandbox-bearer
// handshake headers (Authorization/X-Sandbox-Gen), decoding the response
// into v (if non-nil) -- mirrors postScmCredentialsFull's own shape
// (scmcredentials_integration_test.go).
func postSandboxUpload(t *testing.T, r testRig, path, bearer, gen string, body []byte, v any) int {
	t.Helper()
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, r.server.URL+path, reqBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if gen != "" {
		req.Header.Set("X-Sandbox-Gen", gen)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("decode response body: %v", err)
		}
	}
	return resp.StatusCode
}

// getSandboxUploadRedirect issues a GET against path with the sandbox-bearer
// handshake headers, WITHOUT following any redirect, returning the status
// and (if a 302) the Location header.
func getSandboxUploadRedirect(t *testing.T, r testRig, path, bearer, gen string) (int, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodGet, r.server.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if gen != "" {
		req.Header.Set("X-Sandbox-Gen", gen)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, resp.Header.Get("Location")
}

// getBrowserUploadRedirect is getSandboxUploadRedirect's own cookie-auth
// twin, for the /api-mounted content route.
func getBrowserUploadRedirect(t *testing.T, r testRig, path, token string) (int, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodGet, r.server.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "narvi_auth_session", Value: token})
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, resp.Header.Get("Location")
}

// putBytes PUTs body to putURL with headers set, failing the test on any
// non-2xx response.
func putBytes(t *testing.T, putURL string, headers map[string]string, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build PUT request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do PUT request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("PUT %s status = %d, want 2xx", putURL, resp.StatusCode)
	}
}

// artifactRow re-reads id's own current row directly, for assertions.
func artifactRow(ctx context.Context, t *testing.T, r testRig, id, sessionID pgtype.UUID) sqlcgen.Artifact {
	t.Helper()
	row, err := r.artifacts.GetForSession(ctx, id, sessionID)
	if err != nil {
		t.Fatalf("get artifact row: %v", err)
	}
	return row
}

// artifactEventPayload is the subset of the sandbox-ws Artifact event shape
// these tests assert on -- decoded properly (never via raw string
// Contains) since the events table's payload column is JSONB: Postgres
// re-serializes it on read with its own canonical key order/whitespace
// (always ": "/", " separators, keys reordered), which would make a
// byte-for-byte or substring match against the ORIGINAL json.Marshal
// output fragile and wrong.
type artifactEventPayload struct {
	Type          string  `json:"type"`
	ArtifactType  string  `json:"artifactType"`
	Status        string  `json:"status"`
	FailureReason *string `json:"failureReason"`
}

func decodeArtifactEvent(t *testing.T, payload []byte) artifactEventPayload {
	t.Helper()
	var got artifactEventPayload
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decode artifact event payload %s: %v", payload, err)
	}
	return got
}

// --- Exit criterion: sandbox-shaped mint -> PUT -> confirm -> artifact event observed ---

func TestUploadLifecycle_Sandbox_MintPutConfirmArtifactEvent(t *testing.T) {
	broadcaster := &recordingBroadcaster{}
	rig := newTestRig(t, func(r *testRig) { r.broadcaster = broadcaster })
	ctx := context.Background()

	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	content := []byte("hello from the agent")
	mintBody := []byte(fmt.Sprintf(`{"filename":"report.txt","contentType":"text/plain","sizeBytes":%d}`, len(content)))

	var mint sandboxMintResponse
	status := postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1", mintBody, &mint)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
	}
	if mint.UploadID == "" || mint.PutURL == "" {
		t.Fatalf("mint response = %+v, want non-empty uploadId/putUrl", mint)
	}

	putBytes(t, mint.PutURL, mint.Headers, content)

	var confirm sandboxConfirmResponse
	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/complete", "sandbox-bearer-token", "1", nil, &confirm)
	if status != http.StatusOK {
		t.Fatalf("confirm status = %d, want %d", status, http.StatusOK)
	}
	if confirm.Status != "ready" {
		t.Fatalf("confirm.Status = %q, want %q (failureReason=%v)", confirm.Status, "ready", confirm.FailureReason)
	}

	var uploadID pgtype.UUID
	if err := uploadID.Scan(mint.UploadID); err != nil {
		t.Fatalf("scan upload id: %v", err)
	}
	row := artifactRow(ctx, t, rig, uploadID, session.ID)
	if row.Status != sqlcgen.ArtifactStatusReady {
		t.Errorf("row.Status = %q, want %q", row.Status, sqlcgen.ArtifactStatusReady)
	}
	if !row.CreatedBy.Valid {
		// agent-produced (sandbox-bearer mint): created_by is NULL/invalid,
		// §17.5's own no-human-actor allowance.
	} else {
		t.Errorf("row.CreatedBy = %v, want invalid (agent-produced upload)", row.CreatedBy)
	}

	events, err := rig.events.ListForSession(ctx, session.ID, 0, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "artifact" {
		t.Fatalf("events = %+v, want exactly one 'artifact' event", events)
	}
	eventPayload := decodeArtifactEvent(t, events[0].Payload)
	if eventPayload.ArtifactType != "upload" || eventPayload.Status != "ready" {
		t.Errorf("event payload = %+v, want an upload artifact event with status ready", eventPayload)
	}

	broadcasted := broadcaster.all()
	if len(broadcasted) != 1 {
		t.Fatalf("broadcaster recorded %d events, want 1", len(broadcasted))
	}
	if !strings.Contains(broadcasted[0], session.ID.String()) {
		t.Errorf("broadcast = %q, want it scoped to session %s", broadcasted[0], session.ID.String())
	}
}

// --- Exit criterion: browser-shaped mint -> PUT -> confirm -> download(302) ---

func TestUploadLifecycle_Browser_MintPutConfirmDownload(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	content := []byte("a screenshot's worth of bytes, pretend")
	mintBody := []byte(fmt.Sprintf(`{"filename":"screenshot.png","contentType":"image/png","sizeBytes":%d}`, len(content)))

	var mint restdtos.MintUploadResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/uploads", mintBody, &mint, token)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
	}

	putBytes(t, mint.PutUrl, mint.Headers, content)

	var confirm restdtos.ConfirmUploadResponse
	status = rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/uploads/"+mint.UploadId+"/complete", []byte(`{}`), &confirm, token)
	if status != http.StatusOK {
		t.Fatalf("confirm status = %d, want %d", status, http.StatusOK)
	}
	if string(confirm.Status) != "ready" {
		t.Fatalf("confirm.Status = %q, want %q", confirm.Status, "ready")
	}

	var uploadID pgtype.UUID
	if err := uploadID.Scan(mint.UploadId); err != nil {
		t.Fatalf("scan upload id: %v", err)
	}
	row := artifactRow(ctx, t, rig, uploadID, session.ID)
	if !row.CreatedBy.Valid || row.CreatedBy != owner.ID {
		t.Errorf("row.CreatedBy = %v, want %v (browser upload attributes to the authenticated caller)", row.CreatedBy, owner.ID)
	}
	if row.Url == "" || !strings.HasPrefix(row.Url, "/api/sessions/") {
		t.Errorf("row.Url = %q, want the stable /api/... content path", row.Url)
	}

	redirectStatus, location := getBrowserUploadRedirect(t, rig, "/api/sessions/"+session.ID.String()+"/uploads/"+mint.UploadId+"/content", token)
	if redirectStatus != http.StatusFound {
		t.Fatalf("content redirect status = %d, want %d", redirectStatus, http.StatusFound)
	}
	if location == "" {
		t.Fatal("content redirect Location header is empty")
	}

	// Follow the presigned URL directly (no auth needed against the fake
	// storage origin, matching a real S3-compatible presigned GET) and
	// confirm the bytes round-trip, plus the forced-attachment disposition
	// naming the original filename (§28.5).
	resp, err := http.Get(location) //nolint:gosec // test-only, URL is our own fake server's
	if err != nil {
		t.Fatalf("GET presigned location: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read presigned response body: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded content = %q, want %q", got, content)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "screenshot.png") {
		t.Errorf("Content-Disposition = %q, want attachment naming screenshot.png", cd)
	}
}

// --- Exit criterion: failed-verification case proving failed(reason) +
// outboxed blob_delete + rail-visible event ---

func TestConfirmUpload_VerificationFailed_OutboxesBlobDeleteAndEmitsEvent(t *testing.T) {
	broadcaster := &recordingBroadcaster{}
	rig := newTestRig(t, func(r *testRig) { r.broadcaster = broadcaster })
	ctx := context.Background()

	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	mintBody := []byte(`{"filename":"never-arrives.bin","contentType":"application/octet-stream","sizeBytes":100}`)
	var mint sandboxMintResponse
	status := postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1", mintBody, &mint)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
	}

	// Deliberately never PUT anything -- Stat will observe ErrBlobNotFound,
	// exactly like a client that minted and then failed/gave up mid-transfer.
	var confirm sandboxConfirmResponse
	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/complete", "sandbox-bearer-token", "1", nil, &confirm)
	if status != http.StatusOK {
		t.Fatalf("confirm status = %d, want %d", status, http.StatusOK)
	}
	if confirm.Status != "failed" {
		t.Fatalf("confirm.Status = %q, want %q", confirm.Status, "failed")
	}
	if confirm.FailureReason == nil || *confirm.FailureReason != "verification_failed" {
		t.Fatalf("confirm.FailureReason = %v, want %q", confirm.FailureReason, "verification_failed")
	}

	var uploadID pgtype.UUID
	if err := uploadID.Scan(mint.UploadID); err != nil {
		t.Fatalf("scan upload id: %v", err)
	}
	row := artifactRow(ctx, t, rig, uploadID, session.ID)
	if row.Status != sqlcgen.ArtifactStatusFailed {
		t.Errorf("row.Status = %q, want %q", row.Status, sqlcgen.ArtifactStatusFailed)
	}
	if row.FailureReason == nil || *row.FailureReason != sqlcgen.ArtifactFailureReasonVerificationFailed {
		t.Errorf("row.FailureReason = %v, want %q", row.FailureReason, sqlcgen.ArtifactFailureReasonVerificationFailed)
	}

	// blob_delete was outboxed (§28.4) -- a real external delete is an
	// outbound side effect that must survive a crash, hence the outbox
	// rather than a fire-and-forget call.
	var kind, outboxStatus string
	var payload []byte
	if err := rig.pool.QueryRow(ctx, `SELECT kind, status, payload FROM outbox WHERE session_id = $1`, session.ID).
		Scan(&kind, &outboxStatus, &payload); err != nil {
		t.Fatalf("query outbox row: %v", err)
	}
	if kind != string(ports.NotificationKindBlobDelete) {
		t.Errorf("outbox kind = %q, want %q", kind, ports.NotificationKindBlobDelete)
	}
	var blobDeletePayload struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(payload, &blobDeletePayload); err != nil {
		t.Fatalf("unmarshal blob_delete payload: %v", err)
	}
	if blobDeletePayload.Key == "" {
		t.Error("blob_delete outbox payload has an empty key")
	}

	// rail-visible event: the SAME artifact event every other resolution
	// path appends, replayable via the ordinary events table (§28.6).
	events, err := rig.events.ListForSession(ctx, session.ID, 0, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "artifact" {
		t.Fatalf("events = %+v, want exactly one 'artifact' event", events)
	}
	eventPayload := decodeArtifactEvent(t, events[0].Payload)
	if eventPayload.Status != "failed" {
		t.Errorf("event payload = %+v, want status failed", eventPayload)
	}
	if eventPayload.FailureReason == nil || *eventPayload.FailureReason != "verification_failed" {
		t.Errorf("event payload = %+v, want failureReason verification_failed", eventPayload)
	}

	if len(broadcaster.all()) != 1 {
		t.Fatalf("broadcaster recorded %d events, want 1", len(broadcaster.all()))
	}

	// Idempotency (§28.4): a retried confirm of the now-resolved row
	// returns the SAME recorded outcome, never re-verifies (a second Stat
	// on the still-absent object would be indistinguishable from this
	// path anyway, but the row/event/outbox counts below prove no
	// DOUBLE-append/double-enqueue happened).
	var confirmAgain sandboxConfirmResponse
	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/complete", "sandbox-bearer-token", "1", nil, &confirmAgain)
	if status != http.StatusOK {
		t.Fatalf("retried confirm status = %d, want %d", status, http.StatusOK)
	}
	if confirmAgain.Status != "failed" || confirmAgain.FailureReason == nil || *confirmAgain.FailureReason != "verification_failed" {
		t.Fatalf("retried confirm = %+v, want the SAME recorded outcome", confirmAgain)
	}
	eventsAfterRetry, err := rig.events.ListForSession(ctx, session.ID, 0, 10)
	if err != nil {
		t.Fatalf("list events after retry: %v", err)
	}
	if len(eventsAfterRetry) != 1 {
		t.Errorf("events after retried confirm = %d, want still 1 (never double-appended)", len(eventsAfterRetry))
	}
	var outboxCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1`, session.ID).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxCount != 1 {
		t.Errorf("outbox rows after retried confirm = %d, want still 1 (never double-enqueued)", outboxCount)
	}
}

// --- Exit criterion: oversized-mint refusal (mint-time) ---

func TestMintUpload_OversizedRefusal(t *testing.T) {
	rig := newTestRig(t) // rig.objCfg.MaxUploadBytes == 1024
	ctx := context.Background()

	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	mintBody := []byte(`{"filename":"too-big.bin","contentType":"application/octet-stream","sizeBytes":999999}`)
	status := postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1", mintBody, nil)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("mint status = %d, want %d", status, http.StatusRequestEntityTooLarge)
	}

	// §28.4: "no row, no URL" -- an over-limit mint must never persist
	// anything.
	rows, err := rig.artifacts.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("artifact rows after refused mint = %d, want 0", len(rows))
	}
}

// --- Exit criterion: confirm-time re-check case (two mints racing past the
// session cap: second confirm fails quota_exceeded) ---

func TestConfirmUpload_SessionCapRace_SecondConfirmFailsQuotaExceeded(t *testing.T) {
	// rig.objCfg: MaxUploadBytes=1024, MaxSessionUploadBytes=1500 -- chosen
	// so two individually-within-per-file-cap uploads (1000 bytes each,
	// both <= 1024) still combine to exceed the session cap (2000 > 1500),
	// without either one alone ever tripping size_exceeded.
	fake := newFakeBlobStore(t)
	rig := newTestRig(t, func(r *testRig) { r.blobStore = fake })
	ctx := context.Background()

	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	// Upload A: a real mint+PUT+confirm, landing at 1000 bytes, well under
	// BOTH caps on its own.
	contentA := bytes.Repeat([]byte("a"), 1000)
	var mintA sandboxMintResponse
	status := postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1",
		[]byte(fmt.Sprintf(`{"filename":"a.bin","contentType":"application/octet-stream","sizeBytes":%d}`, len(contentA))), &mintA)
	if status != http.StatusCreated {
		t.Fatalf("mint A status = %d, want %d", status, http.StatusCreated)
	}
	putBytes(t, mintA.PutURL, mintA.Headers, contentA)
	var confirmA sandboxConfirmResponse
	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mintA.UploadID+"/complete", "sandbox-bearer-token", "1", nil, &confirmA)
	if status != http.StatusOK || confirmA.Status != "ready" {
		t.Fatalf("confirm A = (status %d, body %+v), want 200/ready", status, confirmA)
	}

	// Upload B simulates the RACE: minted (via the same real mint
	// endpoint) BEFORE A's own row was committed -- reproduced
	// deterministically here by inserting B's own pending row directly
	// (bypassing mint's own mint-time check, exactly as a real second
	// concurrent mint request would if its OWN SumSessionUploadBytes read
	// happened before A's insert committed) rather than relying on actual
	// goroutine interleaving, which would make this test flaky without
	// exercising any different CONFIRM-time code path. B is sized to pass
	// the PER-FILE cap alone (1000 <= 1024, so its own confirm-time
	// size_exceeded check never fires) while A (already 'ready', 1000) + B
	// together (2000) exceed MaxSessionUploadBytes (1500) -- isolating the
	// SESSION-cap re-check specifically, the thing this test is about.
	contentB := bytes.Repeat([]byte("b"), 1000)
	blobKeyB := "sessions/" + session.ID.String() + "/uploads/race-b"
	fake.put(ports.BlobKey(blobKeyB), contentB)

	var idB pgtype.UUID
	if err := idB.Scan(newDeterministicTestUUID(t, "race-b-"+session.ID.String())); err != nil {
		t.Fatalf("scan upload B id: %v", err)
	}
	sizeB := int64(len(contentB))
	filenameB, contentTypeB, urlB := "b.bin", "application/octet-stream", "/api/sessions/"+session.ID.String()+"/uploads/race-b/content"
	if _, err := rig.artifacts.CreateUpload(ctx, sqlcgen.CreateUploadArtifactParams{
		ID: idB, SessionID: session.ID, Url: urlB, BlobKey: &blobKeyB, SizeBytes: &sizeB,
		ContentType: &contentTypeB, Filename: &filenameB,
	}); err != nil {
		t.Fatalf("directly insert racing upload B: %v", err)
	}

	// A's 1000 (ready) + B's 1000 (this confirm) = 2000 > 1500 (MaxSessionUploadBytes).
	var confirmB sandboxConfirmResponse
	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+idB.String()+"/complete", "sandbox-bearer-token", "1", nil, &confirmB)
	if status != http.StatusOK {
		t.Fatalf("confirm B status = %d, want %d", status, http.StatusOK)
	}
	if confirmB.Status != "failed" {
		t.Fatalf("confirm B.Status = %q, want %q (A=1000 ready + B=1000 should exceed the 1500 session cap)", confirmB.Status, "failed")
	}
	if confirmB.FailureReason == nil || *confirmB.FailureReason != "quota_exceeded" {
		t.Fatalf("confirm B.FailureReason = %v, want %q", confirmB.FailureReason, "quota_exceeded")
	}

	rowB := artifactRow(ctx, t, rig, idB, session.ID)
	if rowB.Status != sqlcgen.ArtifactStatusFailed || rowB.FailureReason == nil || *rowB.FailureReason != sqlcgen.ArtifactFailureReasonQuotaExceeded {
		t.Errorf("row B = (status %q, reason %v), want (failed, quota_exceeded)", rowB.Status, rowB.FailureReason)
	}

	// A's own row is completely unaffected by B's later failure.
	var idA pgtype.UUID
	if err := idA.Scan(mintA.UploadID); err != nil {
		t.Fatalf("scan upload A id: %v", err)
	}
	rowA := artifactRow(ctx, t, rig, idA, session.ID)
	if rowA.Status != sqlcgen.ArtifactStatusReady {
		t.Errorf("row A.Status = %q, want unchanged %q", rowA.Status, sqlcgen.ArtifactStatusReady)
	}
}

// --- attachmentIds wired into createTurnLocked (§28.5) ---

// TestCreateTurn_WithReadyAttachment_RendersAttachmentBlock proves the
// end-to-end path from a real, confirmed upload to a turn's own persisted
// prompt: mint+PUT+confirm an upload to 'ready', then create a turn
// naming it via attachmentIds, and read the turn's own stored prompt back
// to confirm the deterministic attachment block was appended. This rig's
// own objCfg is configured by default (newTestRig's own doc comment), so
// the upload-tool note (FIX D: gated independently, on StorageConfigured,
// turn.go's own createTurnLocked doc comment) rides along too -- both
// happen to be present here, for two DIFFERENT reasons (an attachment was
// named; storage is configured), not because they still share one gate.
func TestCreateTurn_WithReadyAttachment_RendersAttachmentBlock(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	content := []byte("spec details")
	mintBody := []byte(fmt.Sprintf(`{"filename":"spec.txt","contentType":"text/plain","sizeBytes":%d}`, len(content)))
	var mint restdtos.MintUploadResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/uploads", mintBody, &mint, token)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
	}
	putBytes(t, mint.PutUrl, mint.Headers, content)
	var confirm restdtos.ConfirmUploadResponse
	status = rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/uploads/"+mint.UploadId+"/complete", []byte(`{}`), &confirm, token)
	if status != http.StatusOK || string(confirm.Status) != "ready" {
		t.Fatalf("confirm = (status %d, body %+v), want 200/ready", status, confirm)
	}

	turnBody := []byte(fmt.Sprintf(`{"prompt":"please review spec.txt","modelId":null,"effort":null,"planMode":false,"attachmentIds":[%q]}`, mint.UploadId))
	var turnResp restdtos.CreateTurnResponse
	status = rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", turnBody, &turnResp, token)
	if status != http.StatusCreated {
		t.Fatalf("create turn status = %d, want %d", status, http.StatusCreated)
	}

	var turnID pgtype.UUID
	if err := turnID.Scan(turnResp.Id); err != nil {
		t.Fatalf("scan turn id: %v", err)
	}
	turnRow, err := rig.turns.Get(ctx, turnID)
	if err != nil {
		t.Fatalf("get turn row: %v", err)
	}
	if turnRow.Prompt == nil {
		t.Fatal("turn.Prompt is nil")
	}
	prompt := *turnRow.Prompt
	if !strings.Contains(prompt, "please review spec.txt") {
		t.Errorf("turn prompt = %q, want it to still contain the original prompt text", prompt)
	}
	if !strings.Contains(prompt, "spec.txt") || !strings.Contains(prompt, "upload_attachments") {
		t.Errorf("turn prompt = %q, want the rendered attachment block naming spec.txt", prompt)
	}
	if !strings.Contains(prompt, mint.UploadId) {
		t.Errorf("turn prompt = %q, want the attachment's own download path naming its uploadId", prompt)
	}
	// The upload-tool note rides along too -- this rig's own objCfg is
	// configured by default, independently of this turn also naming an
	// attachment (see this test's own doc comment above).
	if !strings.Contains(prompt, "PRODUCE a file") {
		t.Errorf("turn prompt = %q, want the upload-tool note appended (storage is configured in this rig)", prompt)
	}
}

// TestCreateTurn_NoAttachments_StorageConfigured_RendersNoteButNoBlock is
// FIX D's own proof (§28.5, "surfaced to the agent ... in build-turn
// prompts" -- not gated on that same turn also carrying an attachment):
// a turn with NO attachmentIds at all still gets the upload-tool note
// appended, exactly because THIS deployment has object storage configured
// (newTestRig's own default, non-nil objCfg) -- it just gets no attachment
// block, since there is nothing to list. Before FIX D, this was a named,
// accepted gap ("an attachment-free turn never learns it could produce a
// new file"); this test proves the gap is closed.
func TestCreateTurn_NoAttachments_StorageConfigured_RendersNoteButNoBlock(t *testing.T) {
	rig := newTestRig(t) // objCfg configured by default (newTestRig's own doc comment)
	ctx := context.Background()

	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turnBody := []byte(`{"prompt":"do the thing","modelId":null,"effort":null,"planMode":false}`)
	var turnResp restdtos.CreateTurnResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", turnBody, &turnResp, token)
	if status != http.StatusCreated {
		t.Fatalf("create turn status = %d, want %d", status, http.StatusCreated)
	}

	var turnID pgtype.UUID
	if err := turnID.Scan(turnResp.Id); err != nil {
		t.Fatalf("scan turn id: %v", err)
	}
	turnRow, err := rig.turns.Get(ctx, turnID)
	if err != nil {
		t.Fatalf("get turn row: %v", err)
	}
	if turnRow.Prompt == nil {
		t.Fatal("turn.Prompt is nil")
	}
	prompt := *turnRow.Prompt
	if !strings.HasPrefix(prompt, "do the thing") {
		t.Errorf("turn.Prompt = %q, want it to still start with the original prompt text", prompt)
	}
	if strings.Contains(prompt, "upload_attachments") {
		t.Errorf("turn.Prompt = %q, want NO attachment block (no attachmentIds were named)", prompt)
	}
	if !strings.Contains(prompt, "PRODUCE a file") {
		t.Errorf("turn.Prompt = %q, want the upload-tool note appended (storage is configured, independent of attachments)", prompt)
	}
}

// TestCreateTurn_NoAttachments_StorageNotConfigured_ByteForByteNoOp proves
// the ORIGINAL byte-for-byte no-op case still holds when this deployment
// genuinely has no object storage configured at all: neither the
// attachment block nor the upload-tool note renders -- the exact
// invariant this codebase's own workflowengine characterization tests
// (zero-config turns produce byte-identical prompts; CreateTurnCore's
// direct callers there never pass a CreateTurnOptions at all, so
// StorageConfigured is unconditionally false for them regardless of THIS
// rig's own objCfg) depend on.
func TestCreateTurn_NoAttachments_StorageNotConfigured_ByteForByteNoOp(t *testing.T) {
	rig := newTestRig(t, func(r *testRig) {
		r.objCfg = nil
		r.blobStore = nil
	})
	ctx := context.Background()

	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turnBody := []byte(`{"prompt":"do the thing","modelId":null,"effort":null,"planMode":false}`)
	var turnResp restdtos.CreateTurnResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", turnBody, &turnResp, token)
	if status != http.StatusCreated {
		t.Fatalf("create turn status = %d, want %d", status, http.StatusCreated)
	}

	var turnID pgtype.UUID
	if err := turnID.Scan(turnResp.Id); err != nil {
		t.Fatalf("scan turn id: %v", err)
	}
	turnRow, err := rig.turns.Get(ctx, turnID)
	if err != nil {
		t.Fatalf("get turn row: %v", err)
	}
	if turnRow.Prompt == nil || *turnRow.Prompt != "do the thing" {
		t.Errorf("turn.Prompt = %v, want exactly %q (byte-for-byte no-op)", turnRow.Prompt, "do the thing")
	}
}

func TestCreateTurn_WithUnknownAttachment_Returns400(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turnBody := []byte(`{"prompt":"do something","modelId":null,"effort":null,"planMode":false,"attachmentIds":["00000000-0000-0000-0000-000000000000"]}`)
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", turnBody, nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("create turn status = %d, want %d", status, http.StatusBadRequest)
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("turns after rejected create = %d, want 0 (an unknown attachmentId must refuse the whole turn)", len(turns))
	}
}

func TestCreateTurn_WithPendingNotYetReadyAttachment_Returns400(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	mintBody := []byte(`{"filename":"still-uploading.bin","contentType":"application/octet-stream","sizeBytes":10}`)
	var mint restdtos.MintUploadResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/uploads", mintBody, &mint, token)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
	}
	// Deliberately never PUT/confirm -- the row stays 'pending'.

	turnBody := []byte(fmt.Sprintf(`{"prompt":"use the file","modelId":null,"effort":null,"planMode":false,"attachmentIds":[%q]}`, mint.UploadId))
	status = rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", turnBody, nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("create turn status = %d, want %d (attachment is still pending, not ready)", status, http.StatusBadRequest)
	}
}

// --- FIX E (review-fix coverage addition): the sandbox-bearer content
// route -- the download_file tool's own ACTUAL endpoint (§28.5) -- had
// ZERO end-to-end coverage before this batch; getSandboxUploadRedirect
// existed but was never called by any test. ---

// TestUploadContent_Sandbox_RedirectsToPresignedGetWithAttachmentDisposition
// wires getSandboxUploadRedirect into a real test, mirroring
// TestUploadLifecycle_Browser_MintPutConfirmDownload's own tail exactly,
// for the non-/api route.
func TestUploadContent_Sandbox_RedirectsToPresignedGetWithAttachmentDisposition(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	content := []byte("hello from the download_file tool's own real endpoint")
	mintBody := []byte(fmt.Sprintf(`{"filename":"report.txt","contentType":"text/plain","sizeBytes":%d}`, len(content)))
	var mint sandboxMintResponse
	status := postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1", mintBody, &mint)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
	}
	putBytes(t, mint.PutURL, mint.Headers, content)

	var confirm sandboxConfirmResponse
	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/complete", "sandbox-bearer-token", "1", nil, &confirm)
	if status != http.StatusOK || confirm.Status != "ready" {
		t.Fatalf("confirm = (status %d, body %+v), want 200/ready", status, confirm)
	}

	redirectStatus, location := getSandboxUploadRedirect(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/content", "sandbox-bearer-token", "1")
	if redirectStatus != http.StatusFound {
		t.Fatalf("content redirect status = %d, want %d", redirectStatus, http.StatusFound)
	}
	if location == "" {
		t.Fatal("content redirect Location header is empty")
	}

	resp, err := http.Get(location) //nolint:gosec // test-only, URL is our own fake server's
	if err != nil {
		t.Fatalf("GET presigned location: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read presigned response body: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded content = %q, want %q", got, content)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "report.txt") {
		t.Errorf("Content-Disposition = %q, want attachment naming report.txt", cd)
	}
}

// --- FIX F (review-fix coverage addition): the §28.5 404/403/410 +
// negative-auth matrix, untested on all six new routes before this batch. ---

// TestUploadContent_Sandbox_NotReady_Returns404 is table-driven over the
// two non-ready statuses §28.5 collapses to the SAME 404 ("uploadID
// unknown, not this session's, or not status='ready'").
func TestUploadContent_Sandbox_NotReady_Returns404(t *testing.T) {
	tests := []struct {
		name           string
		confirmAndFail bool // whether to confirm (and fail verification) before requesting content
	}{
		{name: "pending (never confirmed)", confirmAndFail: false},
		{name: "failed (confirmed, never uploaded)", confirmAndFail: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()

			session := rig.createSession(ctx, t)
			createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

			mintBody := []byte(`{"filename":"a.bin","contentType":"application/octet-stream","sizeBytes":10}`)
			var mint sandboxMintResponse
			status := postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1", mintBody, &mint)
			if status != http.StatusCreated {
				t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
			}

			if tc.confirmAndFail {
				// Never PUT -- Stat observes ErrBlobNotFound, resolving
				// this row to 'failed'.
				var confirm sandboxConfirmResponse
				status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/complete", "sandbox-bearer-token", "1", nil, &confirm)
				if status != http.StatusOK || confirm.Status != "failed" {
					t.Fatalf("confirm = (status %d, body %+v), want 200/failed", status, confirm)
				}
			}

			redirectStatus, _ := getSandboxUploadRedirect(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/content", "sandbox-bearer-token", "1")
			if redirectStatus != http.StatusNotFound {
				t.Errorf("content status = %d, want %d", redirectStatus, http.StatusNotFound)
			}
		})
	}
}

// TestUploadContent_ForeignSessionUploadID_Returns404 proves session B's
// own bearer cannot fetch session A's own ready upload via the content
// route (path scoped to session B, naming session A's own uploadID) --
// §28.5: "404 ... not this session's".
func TestUploadContent_ForeignSessionUploadID_Returns404(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	sessionA := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, sessionA.ID, "sandbox-bearer-token-a")
	sessionB := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, sessionB.ID, "sandbox-bearer-token-b")

	content := []byte("session A's own file")
	mintBody := []byte(fmt.Sprintf(`{"filename":"a.txt","contentType":"text/plain","sizeBytes":%d}`, len(content)))
	var mint sandboxMintResponse
	status := postSandboxUpload(t, rig, "/sessions/"+sessionA.ID.String()+"/uploads", "sandbox-bearer-token-a", "1", mintBody, &mint)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
	}
	putBytes(t, mint.PutURL, mint.Headers, content)
	var confirm sandboxConfirmResponse
	status = postSandboxUpload(t, rig, "/sessions/"+sessionA.ID.String()+"/uploads/"+mint.UploadID+"/complete", "sandbox-bearer-token-a", "1", nil, &confirm)
	if status != http.StatusOK || confirm.Status != "ready" {
		t.Fatalf("confirm = (status %d, body %+v), want 200/ready", status, confirm)
	}

	// Session B's own bearer, but the URL and uploadID both name session
	// A's own upload.
	redirectStatus, _ := getSandboxUploadRedirect(t, rig, "/sessions/"+sessionB.ID.String()+"/uploads/"+mint.UploadID+"/content", "sandbox-bearer-token-b", "1")
	if redirectStatus != http.StatusNotFound {
		t.Errorf("content status = %d, want %d (session B must never fetch session A's own upload)", redirectStatus, http.StatusNotFound)
	}
}

// TestSandboxBearerUploadRoutes_DeadSandbox_Returns410 mirrors
// TestScmCredentials_DeadSandboxStatus's own exact shape
// (scmcredentials_integration_test.go), for each of the three
// sandbox-bearer upload routes (§28.5's own dead-sandbox handshake,
// shared via sandboxBearerAuth, uploadcore.go).
func TestSandboxBearerUploadRoutes_DeadSandbox_Returns410(t *testing.T) {
	statuses := []struct {
		name   string
		status sqlcgen.SandboxStatus
	}{
		{"stopped", sqlcgen.SandboxStatusStopped},
		{"failed", sqlcgen.SandboxStatusFailed},
	}

	routes := []struct {
		name string
		path string
	}{
		{"mint", "/uploads"},
		{"confirm", "/uploads/00000000-0000-0000-0000-000000000000/complete"},
		{"content", "/uploads/00000000-0000-0000-0000-000000000000/content"},
	}

	for _, rt := range routes {
		for _, tc := range statuses {
			t.Run(rt.name+"/"+tc.name, func(t *testing.T) {
				rig := newTestRig(t)
				ctx := context.Background()

				session := rig.createSession(ctx, t)
				createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")
				moveSandboxStatus(ctx, t, rig, session.ID, tc.status)

				var status int
				if rt.name == "content" {
					status, _ = getSandboxUploadRedirect(t, rig, "/sessions/"+session.ID.String()+rt.path, "sandbox-bearer-token", "1")
				} else {
					status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+rt.path, "sandbox-bearer-token", "1", []byte(`{"filename":"a","contentType":"text/plain","sizeBytes":1}`), nil)
				}
				if status != http.StatusGone {
					t.Errorf("status = %d, want %d (dead sandbox status %s)", status, http.StatusGone, tc.status)
				}
			})
		}
	}
}

// TestSandboxBearerUploadRoutes_BadBearerOrGenMismatch is table-driven
// over the negative-auth matrix (missing bearer, wrong bearer token, gen
// mismatch) across all three sandbox-bearer upload routes.
func TestSandboxBearerUploadRoutes_BadBearerOrGenMismatch(t *testing.T) {
	routes := []struct {
		name string
		path string
	}{
		{"mint", "/uploads"},
		{"confirm", "/uploads/00000000-0000-0000-0000-000000000000/complete"},
		{"content", "/uploads/00000000-0000-0000-0000-000000000000/content"},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			session := rig.createSession(ctx, t)
			createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

			call := func(bearer, gen string) int {
				if rt.name == "content" {
					status, _ := getSandboxUploadRedirect(t, rig, "/sessions/"+session.ID.String()+rt.path, bearer, gen)
					return status
				}
				return postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+rt.path, bearer, gen, []byte(`{"filename":"a","contentType":"text/plain","sizeBytes":1}`), nil)
			}

			t.Run("missing bearer", func(t *testing.T) {
				if got := call("", "1"); got != http.StatusUnauthorized {
					t.Errorf("status = %d, want %d", got, http.StatusUnauthorized)
				}
			})
			t.Run("wrong bearer token", func(t *testing.T) {
				if got := call("wrong-token", "1"); got != http.StatusUnauthorized {
					t.Errorf("status = %d, want %d", got, http.StatusUnauthorized)
				}
			})
			t.Run("gen mismatch", func(t *testing.T) {
				if got := call("sandbox-bearer-token", "999"); got != http.StatusForbidden {
					t.Errorf("status = %d, want %d", got, http.StatusForbidden)
				}
			})
		})
	}
}

// TestMintUploadAPI_ViewerForbidden_Returns403 proves
// authz.ActionUploadToSession's own §13.3 row holds at the REST boundary
// too: a viewer can never mint an upload -- mirrors authorize_test.go's
// own exhaustive-matrix rows for this action (domain/authz package),
// proven end to end here at the HTTP layer.
func TestMintUploadAPI_ViewerForbidden_Returns403(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	_, viewerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleViewer)

	mintBody := []byte(`{"filename":"a.bin","contentType":"application/octet-stream","sizeBytes":10}`)
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/uploads", mintBody, nil, viewerToken)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (a viewer must never mint an upload)", status, http.StatusForbidden)
	}
}

// --- FIX G (review-fix coverage addition): the statErrOverride hook
// existed but was never armed by any test, leaving evaluateConfirmOutcome's
// transient branch, permanent branch, and size-mismatch branch all
// untested. ---

// TestConfirmUpload_TransientStatError_LeavesPendingAndRetryable arms a
// transient Stat error and proves the row stays 'pending' with no
// event/outbox side effect -- the caller gets a retryable 500.
func TestConfirmUpload_TransientStatError_LeavesPendingAndRetryable(t *testing.T) {
	fake := newFakeBlobStore(t)
	rig := newTestRig(t, func(r *testRig) { r.blobStore = fake })
	ctx := context.Background()

	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	mintBody := []byte(`{"filename":"a.bin","contentType":"application/octet-stream","sizeBytes":10}`)
	var mint sandboxMintResponse
	status := postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1", mintBody, &mint)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
	}

	fake.statErrOverride = &ports.BlobStoreError{Transient: true, Code: "http_503", Op: ports.BlobOpStat, Err: errors.New("storage temporarily unavailable")}

	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/complete", "sandbox-bearer-token", "1", nil, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("confirm status = %d, want %d (a transient Stat error must be retryable, not a hard failure)", status, http.StatusInternalServerError)
	}

	var uploadID pgtype.UUID
	if err := uploadID.Scan(mint.UploadID); err != nil {
		t.Fatalf("scan upload id: %v", err)
	}
	row := artifactRow(ctx, t, rig, uploadID, session.ID)
	if row.Status != sqlcgen.ArtifactStatusPending {
		t.Errorf("row.Status = %q, want %q (a transient Stat error must leave the row pending, never failed)", row.Status, sqlcgen.ArtifactStatusPending)
	}

	events, err := rig.events.ListForSession(ctx, session.ID, 0, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("events = %d, want 0 (a still-pending row must never get a resolution event)", len(events))
	}
	var outboxCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1`, session.ID).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxCount != 0 {
		t.Errorf("outbox rows = %d, want 0 (a still-pending row must never get a blob_delete outboxed)", outboxCount)
	}
}

// TestConfirmUpload_PermanentNonNotFoundStatError_LeavesPendingAndCanLaterSucceed
// is written against FIX C's CORRECTED leave-pending behavior (per this
// batch's own instructions): a permanent, non-not-found Stat error
// (401/403/409/413/422 -- e.g. a rotated storage secret or an
// IAM/bucket-policy change) must leave the row pending and retryable,
// EXACTLY like a transient one, never mark it failed -- see
// evaluateConfirmOutcome's own doc comment (uploadconfirm.go) for the full
// fail-safe reasoning this test pins down. Also proves genuine recovery:
// once the permanent-looking error clears (a later confirm with no
// override, against the REAL, already-uploaded object), the row still
// resolves to ready -- proving the earlier error never destroyed anything.
func TestConfirmUpload_PermanentNonNotFoundStatError_LeavesPendingAndCanLaterSucceed(t *testing.T) {
	fake := newFakeBlobStore(t)
	rig := newTestRig(t, func(r *testRig) { r.blobStore = fake })
	ctx := context.Background()

	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	content := []byte("this upload genuinely succeeded")
	mintBody := []byte(fmt.Sprintf(`{"filename":"a.bin","contentType":"application/octet-stream","sizeBytes":%d}`, len(content)))
	var mint sandboxMintResponse
	status := postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1", mintBody, &mint)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
	}
	// The object genuinely, fully arrived -- a rotated credential/policy
	// error at CONFIRM time says nothing about this.
	putBytes(t, mint.PutURL, mint.Headers, content)

	var uploadID pgtype.UUID
	if err := uploadID.Scan(mint.UploadID); err != nil {
		t.Fatalf("scan upload id: %v", err)
	}

	fake.statErrOverride = &ports.BlobStoreError{Transient: false, Code: "http_403", Op: ports.BlobOpStat, Err: errors.New("access denied (simulated rotated credential)")}

	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/complete", "sandbox-bearer-token", "1", nil, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("confirm status = %d, want %d (a permanent, non-not-found Stat error must be retryable, never a hard verification failure)", status, http.StatusInternalServerError)
	}
	row := artifactRow(ctx, t, rig, uploadID, session.ID)
	if row.Status != sqlcgen.ArtifactStatusPending {
		t.Fatalf("row.Status = %q, want %q (FIX C: never destroy a possibly-intact row on an error that doesn't prove absence)", row.Status, sqlcgen.ArtifactStatusPending)
	}
	var outboxCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1`, session.ID).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxCount != 0 {
		t.Fatalf("outbox rows = %d, want 0 (must never outbox a blob_delete for a row that was never proven absent)", outboxCount)
	}

	// "Credentials restored" (no override this time) -- confirm retries
	// against the REAL, already-uploaded object and genuinely recovers.
	var confirmAgain sandboxConfirmResponse
	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/complete", "sandbox-bearer-token", "1", nil, &confirmAgain)
	if status != http.StatusOK || confirmAgain.Status != "ready" {
		t.Fatalf("retried confirm = (status %d, body %+v), want 200/ready (the row was never wrongly destroyed, so it can still resolve to ready)", status, confirmAgain)
	}
	row = artifactRow(ctx, t, rig, uploadID, session.ID)
	if row.Status != sqlcgen.ArtifactStatusReady {
		t.Errorf("row.Status after recovery = %q, want %q", row.Status, sqlcgen.ArtifactStatusReady)
	}
}

// TestConfirmUpload_SizeMismatch_FailsVerificationAndOutboxesBlobDelete
// arms a genuine size mismatch (declared 1000 bytes at mint, only 50 PUT)
// -- evaluateConfirmOutcome's own first branch (statErr == nil &&
// info.SizeBytes != declaredSize), untested before this batch.
func TestConfirmUpload_SizeMismatch_FailsVerificationAndOutboxesBlobDelete(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	mintBody := []byte(`{"filename":"a.bin","contentType":"application/octet-stream","sizeBytes":1000}`)
	var mint sandboxMintResponse
	status := postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1", mintBody, &mint)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
	}
	// A truncated/failed transfer that still leaves SOME object at the
	// key -- declared 1000, actually only 50.
	putBytes(t, mint.PutURL, mint.Headers, bytes.Repeat([]byte("x"), 50))

	var confirm sandboxConfirmResponse
	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/complete", "sandbox-bearer-token", "1", nil, &confirm)
	if status != http.StatusOK {
		t.Fatalf("confirm status = %d, want %d", status, http.StatusOK)
	}
	if confirm.Status != "failed" || confirm.FailureReason == nil || *confirm.FailureReason != "verification_failed" {
		t.Fatalf("confirm = %+v, want failed/verification_failed", confirm)
	}

	var uploadID pgtype.UUID
	if err := uploadID.Scan(mint.UploadID); err != nil {
		t.Fatalf("scan upload id: %v", err)
	}
	row := artifactRow(ctx, t, rig, uploadID, session.ID)
	if row.Status != sqlcgen.ArtifactStatusFailed || row.FailureReason == nil || *row.FailureReason != sqlcgen.ArtifactFailureReasonVerificationFailed {
		t.Errorf("row = (status %q, reason %v), want (failed, verification_failed)", row.Status, row.FailureReason)
	}

	var outboxCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindBlobDelete)).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxCount != 1 {
		t.Errorf("blob_delete outbox rows = %d, want 1", outboxCount)
	}
}

// TestConfirmUpload_ReadyRow_DoubleConfirmIsIdempotent mirrors
// TestConfirmUpload_VerificationFailed_OutboxesBlobDeleteAndEmitsEvent's
// own idempotency proof, for the READY path -- only the failed-path retry
// was tested before this batch (a free defense-in-depth extra: refuted as
// a defect by the review, but worth the coverage).
func TestConfirmUpload_ReadyRow_DoubleConfirmIsIdempotent(t *testing.T) {
	broadcaster := &recordingBroadcaster{}
	rig := newTestRig(t, func(r *testRig) { r.broadcaster = broadcaster })
	ctx := context.Background()

	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	content := []byte("ready and confirmed twice")
	mintBody := []byte(fmt.Sprintf(`{"filename":"a.bin","contentType":"application/octet-stream","sizeBytes":%d}`, len(content)))
	var mint sandboxMintResponse
	status := postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1", mintBody, &mint)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
	}
	putBytes(t, mint.PutURL, mint.Headers, content)

	var confirm sandboxConfirmResponse
	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/complete", "sandbox-bearer-token", "1", nil, &confirm)
	if status != http.StatusOK || confirm.Status != "ready" {
		t.Fatalf("confirm = (status %d, body %+v), want 200/ready", status, confirm)
	}

	var confirmAgain sandboxConfirmResponse
	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mint.UploadID+"/complete", "sandbox-bearer-token", "1", nil, &confirmAgain)
	if status != http.StatusOK || confirmAgain.Status != "ready" {
		t.Fatalf("retried confirm = (status %d, body %+v), want 200/ready (same recorded outcome)", status, confirmAgain)
	}

	var uploadID pgtype.UUID
	if err := uploadID.Scan(mint.UploadID); err != nil {
		t.Fatalf("scan upload id: %v", err)
	}
	row := artifactRow(ctx, t, rig, uploadID, session.ID)
	if row.Status != sqlcgen.ArtifactStatusReady {
		t.Errorf("row.Status = %q, want %q", row.Status, sqlcgen.ArtifactStatusReady)
	}

	events, err := rig.events.ListForSession(ctx, session.ID, 0, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("events after retried confirm = %d, want still 1 (never double-appended)", len(events))
	}
	if len(broadcaster.all()) != 1 {
		t.Errorf("broadcasts after retried confirm = %d, want still 1 (never double-broadcast)", len(broadcaster.all()))
	}
}

// --- FIX K (review-fix coverage addition): a foreign-session
// attachmentId at turn creation was untested (only unknown-uuid and
// pending-not-ready were). Mutation-verified by the reviewer: deleting
// the session_id predicate from ListReadyUploadsByIDsForSession survives
// the whole suite without this test. ---

// TestCreateTurn_ForeignSessionAttachment_Returns400 proves session B
// cannot attach session A's own genuinely-READY upload via attachmentIds
// -- §28.5 names this case explicitly ("of THIS session").
func TestCreateTurn_ForeignSessionAttachment_Returns400(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	ownerA, tokenA := rig.createAuthenticatedUser(ctx, t)
	sessionA := createSessionForUser(ctx, t, rig, ownerA.ID, nil)

	content := []byte("session A's own file")
	mintBody := []byte(fmt.Sprintf(`{"filename":"a.txt","contentType":"text/plain","sizeBytes":%d}`, len(content)))
	var mint restdtos.MintUploadResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+sessionA.ID.String()+"/uploads", mintBody, &mint, tokenA)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want %d", status, http.StatusCreated)
	}
	putBytes(t, mint.PutUrl, mint.Headers, content)
	var confirm restdtos.ConfirmUploadResponse
	status = rig.doJSON(t, http.MethodPost, "/api/sessions/"+sessionA.ID.String()+"/uploads/"+mint.UploadId+"/complete", []byte(`{}`), &confirm, tokenA)
	if status != http.StatusOK || string(confirm.Status) != "ready" {
		t.Fatalf("confirm = (status %d, body %+v), want 200/ready", status, confirm)
	}

	ownerB, tokenB := rig.createAuthenticatedUser(ctx, t)
	sessionB := createSessionForUser(ctx, t, rig, ownerB.ID, nil)

	turnBody := []byte(fmt.Sprintf(`{"prompt":"use it","modelId":null,"effort":null,"planMode":false,"attachmentIds":[%q]}`, mint.UploadId))
	status = rig.doJSON(t, http.MethodPost, "/api/sessions/"+sessionB.ID.String()+"/turns", turnBody, nil, tokenB)
	if status != http.StatusBadRequest {
		t.Fatalf("create turn status = %d, want %d (session B must never attach session A's own upload)", status, http.StatusBadRequest)
	}

	turns, err := rig.turns.ListForSession(ctx, sessionB.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("turns for session B after rejected create = %d, want 0", len(turns))
	}
}

// --- FIX L (review-fix coverage addition): no test proved a failed
// upload frees session-quota headroom (SumSessionUploadBytes excludes
// status='failed', backing the documented retry-after-failure path). ---

// TestMintUpload_AfterFailedUpload_QuotaHeadroomIsFreed mints and fails a
// 1000-byte upload, then proves a second 1000-byte mint is NOT refused:
// if the failed row's bytes still counted toward the session cap (1500,
// this rig's own default), 1000+1000 = 2000 > 1500 would wrongly trip
// quota_exceeded.
func TestMintUpload_AfterFailedUpload_QuotaHeadroomIsFreed(t *testing.T) {
	rig := newTestRig(t) // rig.objCfg: MaxUploadBytes=1024, MaxSessionUploadBytes=1500
	ctx := context.Background()

	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	var mintA sandboxMintResponse
	status := postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1",
		[]byte(`{"filename":"a.bin","contentType":"application/octet-stream","sizeBytes":1000}`), &mintA)
	if status != http.StatusCreated {
		t.Fatalf("mint A status = %d, want %d", status, http.StatusCreated)
	}
	// Never PUT -- confirm resolves this to 'failed' (verification_failed).
	var confirmA sandboxConfirmResponse
	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads/"+mintA.UploadID+"/complete", "sandbox-bearer-token", "1", nil, &confirmA)
	if status != http.StatusOK || confirmA.Status != "failed" {
		t.Fatalf("confirm A = (status %d, body %+v), want 200/failed", status, confirmA)
	}

	var mintB sandboxMintResponse
	status = postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1",
		[]byte(`{"filename":"b.bin","contentType":"application/octet-stream","sizeBytes":1000}`), &mintB)
	if status != http.StatusCreated {
		t.Fatalf("mint B status = %d, want %d (a failed upload's bytes must not count against the session quota)", status, http.StatusCreated)
	}
}

// --- FIX M (review-fix coverage addition): §28.7's own nil-config
// feature-flag path (mint returns a structured 503 "uploads not
// configured") ran in no test -- this rig's own default objCfg is always
// non-nil. ---

// TestMintUpload_NoObjectStorageConfigured_Returns503 nils the config via
// this rig's own existing mutate hook, proving both auth variants answer
// 503, and that nothing else in this rig degrades (session/turn creation
// still work fine).
func TestMintUpload_NoObjectStorageConfigured_Returns503(t *testing.T) {
	rig := newTestRig(t, func(r *testRig) {
		r.objCfg = nil
		r.blobStore = nil
	})
	ctx := context.Background()

	session := rig.createSession(ctx, t)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	mintBody := []byte(`{"filename":"a.bin","contentType":"application/octet-stream","sizeBytes":10}`)
	status := postSandboxUpload(t, rig, "/sessions/"+session.ID.String()+"/uploads", "sandbox-bearer-token", "1", mintBody, nil)
	if status != http.StatusServiceUnavailable {
		t.Errorf("sandbox-bearer mint status = %d, want %d", status, http.StatusServiceUnavailable)
	}

	owner, token := rig.createAuthenticatedUser(ctx, t)
	sessionAPI := createSessionForUser(ctx, t, rig, owner.ID, nil)
	status = rig.doJSON(t, http.MethodPost, "/api/sessions/"+sessionAPI.ID.String()+"/uploads", mintBody, nil, token)
	if status != http.StatusServiceUnavailable {
		t.Errorf("browser mint status = %d, want %d", status, http.StatusServiceUnavailable)
	}

	// Nothing else degrades: an ordinary, attachment-free turn still
	// creates fine on this same session.
	turnBody := []byte(`{"prompt":"do the thing","modelId":null,"effort":null,"planMode":false}`)
	var turnResp restdtos.CreateTurnResponse
	status = rig.doJSON(t, http.MethodPost, "/api/sessions/"+sessionAPI.ID.String()+"/turns", turnBody, &turnResp, token)
	if status != http.StatusCreated {
		t.Errorf("create turn status = %d, want %d (a deployment with no object storage must not degrade unrelated routes)", status, http.StatusCreated)
	}
}

// newDeterministicTestUUID derives a syntactically valid, stable-per-input
// UUID string from seed, without pulling in a real random UUID generator
// for this one racing-fixture id -- any fixed, valid UUID works since
// nothing else in this test ever needs to regenerate the SAME seed twice.
func newDeterministicTestUUID(t *testing.T, seed string) string {
	t.Helper()
	sum := 0
	for _, c := range seed {
		sum = (sum*31 + int(c)) & 0xffff
	}
	return fmt.Sprintf("bbbbbbbb-cccc-dddd-eeee-%012x", sum)
}
