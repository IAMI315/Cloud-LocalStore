package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	"github.com/IAMI315/Cloud-LocalStore/internal/protocol"
)

func objectRequest(t *testing.T, h http.Handler, method, path, device string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("X-Device-ID", device)
	if method != http.MethodGet {
		r.Header.Set("Content-Type", "application/octet-stream")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestObjectUploadExistsDownloadAndRevocation(t *testing.T) {
	app := testApp(t)
	h := app.Handler()
	registerRevisionDevice(t, h, "space", "device")
	data := []byte("hello object")
	hash := objectstore.Hash(data)
	w := objectRequest(t, h, http.MethodPost, "/v1/objects/upload?hash="+string(hash), "device", data)
	if w.Code != http.StatusOK {
		t.Fatalf("upload=%d %s", w.Code, w.Body.String())
	}
	req, _ := json.Marshal(protocol.ObjectExistsRequest{Hashes: []string{string(hash)}})
	r := httptest.NewRequest(http.MethodPost, "/v1/objects/exists", bytes.NewReader(req))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Device-ID", "device")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("exists=%d %s", w.Code, w.Body.String())
	}
	var exists protocol.ObjectExistsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &exists); err != nil || !exists.Exists[string(hash)] {
		t.Fatalf("exists response=%s", w.Body.String())
	}
	w = objectRequest(t, h, http.MethodGet, "/v1/objects/"+string(hash), "device", nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("download=%d %q", w.Code, w.Body.Bytes())
	}
	// A revoked device cannot access objects anymore.
	revoke, _ := json.Marshal(protocol.DeviceRevokeRequest{SpaceID: "space", DeviceID: "device"})
	r = httptest.NewRequest(http.MethodPost, "/v1/devices/revoke", bytes.NewReader(revoke))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Device-ID", "device")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke=%d", w.Code)
	}
	w = objectRequest(t, h, http.MethodGet, "/v1/objects/"+string(hash), "device", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked download=%d", w.Code)
	}
}

func TestCommitRejectsMissingManifestObject(t *testing.T) {
	app := testApp(t)
	h := app.Handler()
	registerRevisionDevice(t, h, "space", "device")
	w := revisionRequest(t, h, http.MethodPost, "/v1/spaces/space/operations/prepare", "device", protocol.OperationPrepareRequest{OperationID: "op", ParentRevision: 0, PathID: "file", OperationType: "update", FileManifestID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if w.Code != http.StatusOK {
		t.Fatalf("prepare=%d %s", w.Code, w.Body.String())
	}
	w = revisionRequest(t, h, http.MethodPost, "/v1/spaces/space/operations/commit", "device", protocol.OperationCommitRequest{OperationID: "op"})
	if w.Code != http.StatusConflict {
		t.Fatalf("commit missing=%d %s", w.Code, w.Body.String())
	}
}
