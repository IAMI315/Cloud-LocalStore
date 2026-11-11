package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/protocol"
)

func testApp(t *testing.T) *App {
	t.Helper()
	var err error
	app, err := NewApp(Config{DataDir: t.TempDir(), DatabasePath: ":memory:", ListenAddr: "127.0.0.1:0", DevMode: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	return app
}

func TestServiceRegisterStateAndReady(t *testing.T) {
	app := testApp(t)
	h := app.Handler()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("health=%d", w.Code)
	}
	body, _ := json.Marshal(protocol.DeviceRegisterRequest{SpaceID: "space", DeviceID: "device", Name: "laptop"})
	r = httptest.NewRequest(http.MethodPost, "/v1/devices/register", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("register=%d body=%s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest(http.MethodGet, "/v1/spaces/space/state", nil)
	r.Header.Set("X-Device-ID", "device")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("state=%d body=%s", w.Code, w.Body.String())
	}
	if err := app.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPairingRequiresTokenAndBearerCredential(t *testing.T) {
	root := t.TempDir()
	app, err := NewApp(Config{DataDir: root, DatabasePath: filepath.Join(root, "metadata.db"), ListenAddr: "127.0.0.1:0", PairingToken: "pair-secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	h := app.Handler()
	register := func(token string) (int, protocol.DeviceRegisterResponse) {
		body, _ := json.Marshal(protocol.DeviceRegisterRequest{SpaceID: "space", DeviceID: "device", Name: "laptop", PairingToken: token})
		r := httptest.NewRequest(http.MethodPost, "/v1/devices/register", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out protocol.DeviceRegisterResponse
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, _ := register(""); code != http.StatusUnauthorized {
		t.Fatalf("missing pairing code=%d", code)
	}
	code, out := register("pair-secret")
	if code != http.StatusOK || out.DeviceToken == "" {
		t.Fatalf("pair code=%d response=%+v", code, out)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/spaces/space/state", nil)
	r.Header.Set("X-Device-ID", "device")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer code=%d", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, "/v1/spaces/space/state", nil)
	r.Header.Set("X-Device-ID", "device")
	r.Header.Set("Authorization", "Bearer "+out.DeviceToken)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("valid bearer code=%d body=%s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest(http.MethodPost, "/v1/pairing/start", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("anonymous pairing endpoint code=%d", w.Code)
	}
}

func TestServiceRejectsWrongMethodAndUnauthorized(t *testing.T) {
	app := testApp(t)
	h := app.Handler()
	r := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method=%d", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, "/v1/spaces/space/state", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("auth=%d", w.Code)
	}
}

func TestServiceUnknownWorkspaceDoesNotLeakWithoutAuthorization(t *testing.T) {
	app := testApp(t)
	h := app.Handler()
	r := httptest.NewRequest(http.MethodGet, "/v1/spaces/missing/state", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth=%d", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, "/v1/spaces/missing/state", nil)
	r.Header.Set("X-Device-ID", "unknown")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong auth=%d", w.Code)
	}
	body, _ := json.Marshal(protocol.DeviceRegisterRequest{SpaceID: "known", DeviceID: "device", Name: "laptop"})
	r = httptest.NewRequest(http.MethodPost, "/v1/devices/register", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	r = httptest.NewRequest(http.MethodGet, "/v1/spaces/missing/state", nil)
	r.Header.Set("X-Device-ID", "device")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("authorized unknown=%d", w.Code)
	}
}

func TestServiceOperationRevisionLifecycle(t *testing.T) {
	app := testApp(t)
	h := app.Handler()
	register := func(space, device string) {
		body, _ := json.Marshal(protocol.DeviceRegisterRequest{SpaceID: space, DeviceID: device, Name: device})
		r := httptest.NewRequest(http.MethodPost, "/v1/devices/register", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("register=%d body=%s", w.Code, w.Body.String())
		}
	}
	register("space", "device")

	prepare := func(path, operationID string, parent int64) (int, protocol.Operation) {
		body, _ := json.Marshal(protocol.OperationPrepareRequest{
			OperationID: operationID, ParentRevision: parent, PathID: path,
			OperationType: "update", DeviceLocalSequence: parent + 1,
		})
		r := httptest.NewRequest(http.MethodPost, "/v1/spaces/space/operations/prepare", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Device-ID", "device")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var response protocol.OperationPrepareResponse
		_ = json.Unmarshal(w.Body.Bytes(), &response)
		return w.Code, response.Operation
	}
	if code, op := prepare("file.txt", "op-1", 0); code != http.StatusOK || op.State != "prepared" {
		t.Fatalf("prepare code=%d op=%+v", code, op)
	}
	if code, op := prepare("file.txt", "op-1", 0); code != http.StatusOK || op.State != "prepared" {
		t.Fatalf("idempotent prepare code=%d op=%+v", code, op)
	}
	if code, _ := prepare("other.txt", "op-1", 0); code != http.StatusConflict {
		t.Fatalf("payload reuse code=%d", code)
	}

	commitBody, _ := json.Marshal(protocol.OperationCommitRequest{OperationID: "op-1"})
	r := httptest.NewRequest(http.MethodPost, "/v1/spaces/space/operations/commit", bytes.NewReader(commitBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Device-ID", "device")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("commit=%d body=%s", w.Code, w.Body.String())
	}
	var committed protocol.OperationCommitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &committed); err != nil || committed.Operation.Revision != 1 {
		t.Fatalf("commit response=%s", w.Body.String())
	}

	r = httptest.NewRequest(http.MethodGet, "/v1/spaces/space/operations?after_revision=0", nil)
	r.Header.Set("X-Device-ID", "device")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list=%d body=%s", w.Code, w.Body.String())
	}
	var listed protocol.OperationListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed.Operations) != 1 || listed.CurrentRevision != 1 {
		t.Fatalf("list response=%s", w.Body.String())
	}

	ackBody, _ := json.Marshal(protocol.AckRequest{AckRevision: 1, LastSeenRevision: 1, LastAppliedRevision: 1})
	r = httptest.NewRequest(http.MethodPost, "/v1/spaces/space/ack", bytes.NewReader(ackBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Device-ID", "device")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("ack=%d body=%s", w.Code, w.Body.String())
	}
}
