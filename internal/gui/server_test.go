package gui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

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
