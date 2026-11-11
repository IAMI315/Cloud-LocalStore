package sync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
	"github.com/IAMI315/Cloud-LocalStore/internal/filesystem"
	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	"github.com/IAMI315/Cloud-LocalStore/internal/protocol"
	"github.com/IAMI315/Cloud-LocalStore/internal/server"
	"github.com/IAMI315/Cloud-LocalStore/internal/store"
	_ "modernc.org/sqlite"
)

func registerHTTPDevice(t *testing.T, baseURL, space, device, pairingToken string, client *http.Client) string {
	t.Helper()
	body, err := json.Marshal(protocol.DeviceRegisterRequest{SpaceID: space, DeviceID: device, Name: device, PairingToken: pairingToken})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/devices/register", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register %s=%d", device, resp.StatusCode)
	}
	var out protocol.DeviceRegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.DeviceToken
}

func TestHTTPRemoteTwoDeviceSync(t *testing.T) {
	serverRoot := t.TempDir()
	app, err := server.NewApp(server.Config{DataDir: serverRoot, DatabasePath: filepath.Join(serverRoot, "metadata.db"), ListenAddr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	client := ts.Client()
	tokenA := registerHTTPDevice(t, ts.URL, "space", "device-a", app.PairingToken(), client)
	tokenB := registerHTTPDevice(t, ts.URL, "space", "device-b", app.PairingToken(), client)

	newEngine := func(device, token string) (*Engine, string) {
		t.Helper()
		root := t.TempDir()
		db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "state.db")+"?mode=rwc")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		index, err := store.OpenClient(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		objects, err := objectstore.NewLocalFS(filepath.Join(t.TempDir(), "objects"))
		if err != nil {
			t.Fatal(err)
		}
		remote := &objectstore.RemoteClient{BaseURL: ts.URL, DeviceID: device, DeviceToken: token, SpaceID: "space", HTTP: client}
		engine, err := NewEngine(root, "space", device, objects, index, remote)
		if err != nil {
			t.Fatal(err)
		}
		return engine, root
	}

	a, rootA := newEngine("device-a", tokenA)
	if err := os.WriteFile(filepath.Join(rootA, "shared.txt"), []byte("from device A"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	b, rootB := newEngine("device-b", tokenB)
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(rootB, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from device A" {
		t.Fatalf("device B content=%q", got)
	}
	if err := os.WriteFile(filepath.Join(rootA, "shared.txt"), []byte("from device A again"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(rootB, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from device A again" {
		t.Fatalf("offline catch-up content=%q", got)
	}

	if err := os.WriteFile(filepath.Join(rootB, "shared.txt"), []byte("from device B conflict"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := (Scanner{FS: filesystem.OSFileSystem{}, Root: rootB}).ScanContext(context.Background())
	if err != nil || len(entries) != 1 {
		t.Fatalf("scan device B=%+v err=%v", entries, err)
	}
	localManifest, err := objectstore.BuildFile(context.Background(), filepath.Join(rootB, "shared.txt"), b.Objects, domain.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	localManifestID, err := b.Objects.PutManifest(context.Background(), localManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.recordLocalManifest(context.Background(), entries[0], localManifest, localManifestID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootA, "shared.txt"), []byte("from device A wins"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(rootB, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from device A wins" {
		t.Fatalf("conflict primary content=%q", got)
	}
	conflicts, err := filepath.Glob(filepath.Join(rootB, "shared.conflict-device-b-r*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflict files=%v", conflicts)
	}
	got, err = os.ReadFile(conflicts[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from device B conflict" {
		t.Fatalf("conflict copy content=%q", got)
	}
}
