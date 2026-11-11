package gui

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/localapp"
	"github.com/IAMI315/Cloud-LocalStore/internal/protocol"
)

func TestSelectFolderAPI(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		server := New("")
		server.selectFolder = func() (string, error) { return "selected-folder", nil }
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/select-folder", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		expectedPath, err := filepath.Abs("selected-folder")
		if err != nil {
			t.Fatal(err)
		}
		if body["selected"] != true || body["cancelled"] != false || body["path"] != expectedPath {
			t.Fatalf("body = %#v", body)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		server := New("")
		server.selectFolder = func() (string, error) { return "", nil }
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/select-folder", nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cancelled":true`) {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	})
	t.Run("unsupported platform", func(t *testing.T) {
		server := New("")
		server.selectFolder = func() (string, error) { return "", ErrFolderSelectionUnsupported }
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/select-folder", nil))
		if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "手动填写路径") {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	})
	t.Run("method", func(t *testing.T) {
		server := New("")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/select-folder", nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	})
}

func TestEmbeddedFolderPickerControls(t *testing.T) {
	index, err := fs.ReadFile(webFiles, "web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(webFiles, "web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `id="browse"`) || !strings.Contains(string(index), `id="directory-tree"`) || !strings.Contains(string(app), "/api/select-folder") || !strings.Contains(string(app), "setInterval") {
		t.Fatal("embedded page is missing folder picker controls")
	}
}

func TestHTTPAPIRequiresLibraryAndSupportsLocalWorkflow(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("version one"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := New("")
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer server.Close()

	status := getJSON(t, httpServer.URL+"/api/state")
	if status.Code != http.StatusOK || status.Body["configured"] != false {
		t.Fatalf("initial state = %#v", status)
	}
	scan := postJSON(t, httpServer.URL+"/api/scan", map[string]any{})
	if scan.Code != http.StatusConflict {
		t.Fatalf("scan without library status = %d, body=%s", scan.Code, scan.Raw)
	}
	opened := postJSON(t, httpServer.URL+"/api/library", map[string]any{"root": root, "initialize": true})
	if opened.Code != http.StatusOK {
		t.Fatalf("open status = %d, body=%s", opened.Code, opened.Raw)
	}
	scan = postJSON(t, httpServer.URL+"/api/scan", map[string]any{})
	if scan.Code != http.StatusOK {
		t.Fatalf("scan status = %d, body=%s", scan.Code, scan.Raw)
	}
	files := getJSON(t, httpServer.URL+"/api/files")
	if files.Code != http.StatusOK || len(files.Array) != 1 {
		t.Fatalf("files = %#v", files)
	}
	versions := getJSON(t, httpServer.URL+"/api/versions?path=hello.txt")
	if versions.Code != http.StatusOK || len(versions.Array) != 1 {
		t.Fatalf("versions = %#v", versions)
	}
	versionID, ok := versions.Array[0].(map[string]any)["ID"].(string)
	if !ok || versionID == "" {
		t.Fatalf("version response = %#v", versions.Array[0])
	}
	restored := postJSON(t, httpServer.URL+"/api/restore", map[string]any{"path": "hello.txt", "version": versionID})
	if restored.Code != http.StatusOK {
		t.Fatalf("restore status = %d, body=%s", restored.Code, restored.Raw)
	}
	if monitor := postJSON(t, httpServer.URL+"/api/monitor", map[string]any{"action": "start"}); monitor.Code != http.StatusOK {
		t.Fatalf("monitor start status = %d, body=%s", monitor.Code, monitor.Raw)
	}
	if monitor := postJSON(t, httpServer.URL+"/api/monitor", map[string]any{"action": "stop"}); monitor.Code != http.StatusOK {
		t.Fatalf("monitor stop status = %d, body=%s", monitor.Code, monitor.Raw)
	}
	invalid := getJSON(t, httpServer.URL+"/api/versions?path=../outside.txt")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid path status = %d, body=%s", invalid.Code, invalid.Raw)
	}
	if method := getJSON(t, httpServer.URL+"/api/scan"); method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method error status = %d", method.Code)
	}
}

func TestHTTPAPIRejectsMismatchedDataDirectory(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	server := New("")
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer server.Close()
	if got := postJSON(t, httpServer.URL+"/api/library", map[string]any{"root": root, "initialize": true}).Code; got != http.StatusOK {
		t.Fatalf("initialize status = %d", got)
	}
	if got := postJSON(t, httpServer.URL+"/api/library", map[string]any{"root": root, "data_dir": other}).Code; got != http.StatusBadRequest {
		t.Fatalf("mismatched data dir status = %d", got)
	}
}

func TestHTTPAPIConflictWorkflow(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "report.txt"), []byte("remote"), 0o600); err != nil {
		t.Fatal(err)
	}
	conflictPath := filepath.Join(root, "report.conflict-device-a-r7.txt")
	if err := os.WriteFile(conflictPath, []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := New("")
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer server.Close()
	if opened := postJSON(t, httpServer.URL+"/api/library", map[string]any{"root": root, "initialize": true}); opened.Code != http.StatusOK {
		t.Fatalf("open status = %d, body=%s", opened.Code, opened.Raw)
	}
	conflicts := getJSON(t, httpServer.URL+"/api/conflicts")
	if conflicts.Code != http.StatusOK || len(conflicts.Array) != 1 {
		t.Fatalf("conflicts = %#v", conflicts)
	}
	item, ok := conflicts.Array[0].(map[string]any)
	if !ok || item["original_path"] != "report.txt" {
		t.Fatalf("conflict item = %#v", item)
	}
	resolved := postJSON(t, httpServer.URL+"/api/conflicts/resolve", map[string]any{"path": "report.conflict-device-a-r7.txt", "action": "keep_local"})
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve status = %d, body=%s", resolved.Code, resolved.Raw)
	}
	data, err := os.ReadFile(filepath.Join(root, "report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "local" || fileExists(conflictPath) {
		t.Fatalf("keep_local result content=%q conflict_exists=%v", data, fileExists(conflictPath))
	}

	secondConflict := filepath.Join(root, "report.conflict-device-a-r8.txt")
	if err := os.WriteFile(secondConflict, []byte("discard me"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved = postJSON(t, httpServer.URL+"/api/conflicts/resolve", map[string]any{"path": "report.conflict-device-a-r8.txt", "action": "keep_remote"})
	if resolved.Code != http.StatusOK || fileExists(secondConflict) {
		t.Fatalf("keep_remote status=%d body=%s conflict_exists=%v", resolved.Code, resolved.Raw, fileExists(secondConflict))
	}
}

func TestConnectionSavePreservesSecretsWhenPasswordFieldsAreBlank(t *testing.T) {
	root := t.TempDir()
	server := New("")
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer server.Close()
	if got := postJSON(t, httpServer.URL+"/api/library", map[string]any{"root": root, "initialize": true}); got.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body=%s", got.Code, got.Raw)
	}
	first := postJSON(t, httpServer.URL+"/api/connection", map[string]any{
		"server_url": "http://example.invalid:18080", "space_id": "demo", "device_id": "laptop",
		"device_token": "device-secret", "pairing_token": "pair-secret",
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first connection status = %d, body=%s", first.Code, first.Raw)
	}
	second := postJSON(t, httpServer.URL+"/api/connection", map[string]any{
		"server_url": "http://example.invalid:18080", "space_id": "demo", "device_id": "laptop",
	})
	if second.Code != http.StatusOK {
		t.Fatalf("second connection status = %d, body=%s", second.Code, second.Raw)
	}
	if stopped := postJSON(t, httpServer.URL+"/api/monitor", map[string]any{"action": "stop"}); stopped.Code != http.StatusOK {
		t.Fatalf("monitor stop status = %d, body=%s", stopped.Code, stopped.Raw)
	}
	cfg, err := localapp.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceToken != "device-secret" || cfg.PairingToken != "pair-secret" {
		t.Fatalf("secrets were not preserved: device=%q pairing=%q", cfg.DeviceToken, cfg.PairingToken)
	}
}

func TestConnectionSavePairsDeviceWithServer(t *testing.T) {
	var received protocol.DeviceRegisterRequest
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/devices/register" || r.Method != http.MethodPost {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.DeviceRegisterResponse{
			Device:      protocol.Device{SpaceID: received.SpaceID, DeviceID: received.DeviceID, Name: received.Name, Status: "active"},
			DeviceToken: "issued-device-token",
		})
	}))
	defer remote.Close()
	root := t.TempDir()
	server := New("")
	local := httptest.NewServer(server.Handler())
	defer local.Close()
	defer server.Close()
	if got := postJSON(t, local.URL+"/api/library", map[string]any{"root": root, "initialize": true}); got.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body=%s", got.Code, got.Raw)
	}
	paired := postJSON(t, local.URL+"/api/connection", map[string]any{
		"server_url": remote.URL, "space_id": "demo", "device_id": "laptop", "pairing_token": "pair-secret",
	})
	if paired.Code != http.StatusOK {
		t.Fatalf("pair status = %d, body=%s", paired.Code, paired.Raw)
	}
	if received.SpaceID != "demo" || received.DeviceID != "laptop" || received.PairingToken != "pair-secret" {
		t.Fatalf("unexpected registration request: %+v", received)
	}
	cfg, err := localapp.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceToken != "issued-device-token" {
		t.Fatalf("device token was not saved: %q", cfg.DeviceToken)
	}
}

func TestConnectionSaveRepairsDeviceWhenPairingCodeIsSupplied(t *testing.T) {
	registrationCount := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/devices/register" || r.Method != http.MethodPost {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		registrationCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.DeviceRegisterResponse{DeviceToken: "fresh-device-token"})
	}))
	defer remote.Close()
	root := t.TempDir()
	server := New("")
	local := httptest.NewServer(server.Handler())
	defer local.Close()
	defer server.Close()
	if got := postJSON(t, local.URL+"/api/library", map[string]any{"root": root, "initialize": true}); got.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body=%s", got.Code, got.Raw)
	}
	first := postJSON(t, local.URL+"/api/connection", map[string]any{
		"server_url": remote.URL, "space_id": "demo", "device_id": "desktop-local", "device_token": "stale-device-token",
	})
	if first.Code != http.StatusOK {
		t.Fatalf("initial connection status = %d, body=%s", first.Code, first.Raw)
	}
	second := postJSON(t, local.URL+"/api/connection", map[string]any{
		"server_url": remote.URL, "space_id": "demo", "device_id": "desktop-local", "pairing_token": "pair-secret",
	})
	if second.Code != http.StatusOK {
		t.Fatalf("repair connection status = %d, body=%s", second.Code, second.Raw)
	}
	if registrationCount != 1 {
		t.Fatalf("registration count = %d, want 1", registrationCount)
	}
	cfg, err := localapp.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceToken != "fresh-device-token" {
		t.Fatalf("fresh device token was not saved: %q", cfg.DeviceToken)
	}
}

func TestOpenDefaultPreservesConnectionAcrossRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "CloudLocalStore-Sync")
	dataDir := filepath.Join(t.TempDir(), "CloudLocalStore-State")
	first := New("")
	if err := first.OpenDefault(root, dataDir); err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(first.Handler())
	connection := postJSON(t, local.URL+"/api/connection", map[string]any{
		"server_url": "http://186.244.241.96:18080", "space_id": "demo", "device_id": "desktop-a",
		"device_token": "persistent-device-token", "pairing_token": "persistent-pairing-token",
	})
	local.Close()
	if connection.Code != http.StatusOK {
		first.Close()
		t.Fatalf("save connection status = %d, body=%s", connection.Code, connection.Raw)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := New("")
	defer second.Close()
	if err := second.OpenDefault(root, dataDir); err != nil {
		t.Fatal(err)
	}
	cfg, err := localapp.LoadConfigFromData(root, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceToken != "persistent-device-token" || cfg.PairingToken != "persistent-pairing-token" {
		t.Fatalf("connection credentials were reset: device=%q pairing=%q", cfg.DeviceToken, cfg.PairingToken)
	}
}

func TestListenRejectsNonLoopbackAddress(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":8765", "192.0.2.10:8765"} {
		server := New(addr)
		listener, err := server.Listen()
		if err == nil {
			_ = listener.Close()
			t.Fatalf("Listen(%q) unexpectedly succeeded", addr)
		}
	}
	server := New("127.0.0.1:0")
	listener, err := server.Listen()
	if err != nil {
		t.Fatalf("loopback Listen failed: %v", err)
	}
	_ = listener.Close()
}

type response struct {
	Code  int
	Body  map[string]any
	Array []any
	Raw   string
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func getJSON(t *testing.T, url string) response {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return readResponse(t, res)
}

func postJSON(t *testing.T, url string, value any) response {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return readResponse(t, res)
}

func readResponse(t *testing.T, res *http.Response) response {
	t.Helper()
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := response{Code: res.StatusCode, Raw: string(body)}
	if len(body) > 0 {
		if body[0] == '[' {
			if err := json.Unmarshal(body, &out.Array); err != nil {
				t.Fatalf("decode array response: %v", err)
			}
		} else if err := json.Unmarshal(body, &out.Body); err != nil {
			t.Fatalf("decode object response: %v", err)
		}
	}
	return out
}
