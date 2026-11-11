package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/protocol"
)

func revisionRequest(t *testing.T, h http.Handler, method, path, device string, value any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Device-ID", device)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func registerRevisionDevice(t *testing.T, h http.Handler, space, device string) {
	t.Helper()
	w := revisionRequest(t, h, http.MethodPost, "/v1/devices/register", "", protocol.DeviceRegisterRequest{SpaceID: space, DeviceID: device, Name: device})
	if w.Code != http.StatusOK {
		t.Fatalf("register %s: %d %s", device, w.Code, w.Body.String())
	}
}

func TestOperationIdempotencyAndRevision(t *testing.T) {
	app := testApp(t)
	h := app.Handler()
	registerRevisionDevice(t, h, "space", "device")
	req := protocol.OperationPrepareRequest{OperationID: "op-1", ParentRevision: 0, PathID: "file-1", OperationType: "update", FileManifestID: "manifest-1", DeviceLocalSequence: 1}
	w := revisionRequest(t, h, http.MethodPost, "/v1/spaces/space/operations/prepare", "device", req)
	if w.Code != http.StatusOK {
		t.Fatalf("prepare: %d %s", w.Code, w.Body.String())
	}
	var prepared protocol.OperationPrepareResponse
	if err := json.Unmarshal(w.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.Operation.State != "prepared" || prepared.Operation.Revision != 0 {
		t.Fatalf("prepared=%+v", prepared.Operation)
	}
	w = revisionRequest(t, h, http.MethodPost, "/v1/spaces/space/operations/commit", "device", protocol.OperationCommitRequest{OperationID: "op-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("commit: %d %s", w.Code, w.Body.String())
	}
	var committed protocol.OperationCommitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &committed); err != nil {
		t.Fatal(err)
	}
	if committed.Operation.Revision != 1 {
		t.Fatalf("revision=%d", committed.Operation.Revision)
	}
	w = revisionRequest(t, h, http.MethodPost, "/v1/spaces/space/operations/commit", "device", protocol.OperationCommitRequest{OperationID: "op-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("idempotent commit: %d", w.Code)
	}
	w = revisionRequest(t, h, http.MethodPost, "/v1/spaces/space/operations/prepare", "device", protocol.OperationPrepareRequest{OperationID: "op-1", ParentRevision: 0, PathID: "different", OperationType: "update", DeviceLocalSequence: 1})
	if w.Code != http.StatusConflict {
		t.Fatalf("payload reuse: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/spaces/space/operations?after_revision=0", nil)
	r.Header.Set("X-Device-ID", "device")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	var listed protocol.OperationListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Operations) != 1 || listed.Operations[0].Revision != 1 {
		t.Fatalf("listed=%+v", listed)
	}
}

func TestOperationAuthorizationAndRevocation(t *testing.T) {
	app := testApp(t)
	h := app.Handler()
	registerRevisionDevice(t, h, "space", "one")
	registerRevisionDevice(t, h, "space", "two")
	req := protocol.OperationPrepareRequest{OperationID: "op-1", ParentRevision: 0, PathID: "file", OperationType: "create"}
	if w := revisionRequest(t, h, http.MethodPost, "/v1/spaces/space/operations/prepare", "one", req); w.Code != http.StatusOK {
		t.Fatalf("prepare: %d", w.Code)
	}
	if w := revisionRequest(t, h, http.MethodPost, "/v1/spaces/space/operations/commit", "two", protocol.OperationCommitRequest{OperationID: "op-1"}); w.Code != http.StatusConflict {
		t.Fatalf("cross-device commit: %d", w.Code)
	}
	if w := revisionRequest(t, h, http.MethodPost, "/v1/devices/revoke", "one", protocol.DeviceRevokeRequest{SpaceID: "space", DeviceID: "one"}); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w := revisionRequest(t, h, http.MethodPost, "/v1/spaces/space/operations/commit", "one", protocol.OperationCommitRequest{OperationID: "op-1"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked commit: %d", w.Code)
	}
	if w := revisionRequest(t, h, http.MethodPost, "/v1/spaces/space/ack", "one", protocol.AckRequest{AckRevision: 0}); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked ack: %d", w.Code)
	}
}

func TestConcurrentOperationCommitProducesOneRevision(t *testing.T) {
	app := testApp(t)
	h := app.Handler()
	registerRevisionDevice(t, h, "space", "device")
	if w := revisionRequest(t, h, http.MethodPost, "/v1/spaces/space/operations/prepare", "device", protocol.OperationPrepareRequest{OperationID: "op-1", ParentRevision: 0, PathID: "file", OperationType: "create"}); w.Code != http.StatusOK {
		t.Fatalf("prepare: %d", w.Code)
	}
	results := make([]int, 2)
	bodies := make([]protocol.OperationCommitResponse, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range results {
		go func(i int) {
			defer wg.Done()
			w := revisionRequest(t, h, http.MethodPost, "/v1/spaces/space/operations/commit", "device", protocol.OperationCommitRequest{OperationID: "op-1"})
			results[i] = w.Code
			if w.Code == http.StatusOK {
				_ = json.Unmarshal(w.Body.Bytes(), &bodies[i])
			}
		}(i)
	}
	wg.Wait()
	for i, code := range results {
		if code != http.StatusOK {
			t.Fatalf("commit %d: %d", i, code)
		}
		if bodies[i].Operation.Revision != 1 {
			t.Fatalf("commit %d revision=%d", i, bodies[i].Operation.Revision)
		}
	}
}
