package objectstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
	"github.com/IAMI315/Cloud-LocalStore/internal/protocol"
)

// RemoteClient is the minimal client used by the agent to exchange CAS
// objects with a VPS. It deliberately contains no watcher or reconciliation
// loop; callers can compose it with the local object store and sync planner.
type RemoteClient struct {
	BaseURL     string
	DeviceID    string
	SpaceID     string
	DeviceToken string
	HTTP        *http.Client
}

var defaultRemoteHTTPClient = &http.Client{Timeout: 30 * time.Second}

// RegisterDevice pairs a new device with the server and returns its bearer
// credential. The pairing token is sent only to the registration endpoint.
func (c *RemoteClient) RegisterDevice(ctx context.Context, name, pairingToken string) (protocol.DeviceRegisterResponse, error) {
	var out protocol.DeviceRegisterResponse
	req := protocol.DeviceRegisterRequest{SpaceID: c.SpaceID, DeviceID: c.DeviceID, Name: name, PairingToken: pairingToken}
	if err := c.postJSON(ctx, "/v1/devices/register", req, &out); err != nil {
		return out, err
	}
	return out, nil
}

// PrepareOperation, CommitOperation, ListOperations and Ack expose the
// revision protocol through the same context-aware client used for objects.
func (c *RemoteClient) PrepareOperation(ctx context.Context, req protocol.OperationPrepareRequest) (protocol.Operation, error) {
	var out protocol.OperationPrepareResponse
	if err := c.postJSON(ctx, "/v1/spaces/"+c.SpaceID+"/operations/prepare", req, &out); err != nil {
		return protocol.Operation{}, err
	}
	return out.Operation, nil
}

func (c *RemoteClient) CommitOperation(ctx context.Context, req protocol.OperationCommitRequest) (protocol.Operation, error) {
	var out protocol.OperationCommitResponse
	if err := c.postJSON(ctx, "/v1/spaces/"+c.SpaceID+"/operations/commit", req, &out); err != nil {
		return protocol.Operation{}, err
	}
	return out.Operation, nil
}

func (c *RemoteClient) ListOperations(ctx context.Context, after int64) (protocol.OperationListResponse, error) {
	var out protocol.OperationListResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/v1/spaces/"+c.SpaceID+"/operations?after_revision="+fmt.Sprint(after)), nil)
	if err != nil {
		return out, err
	}
	c.auth(req)
	resp, err := c.client().Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return out, readRemoteError(resp)
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}

func (c *RemoteClient) Ack(ctx context.Context, req protocol.AckRequest) (protocol.AckResponse, error) {
	var out protocol.AckResponse
	if err := c.postJSON(ctx, "/v1/spaces/"+c.SpaceID+"/ack", req, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *RemoteClient) postJSON(ctx context.Context, path string, value, out any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(path), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readRemoteError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *RemoteClient) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultRemoteHTTPClient
}

func (c *RemoteClient) endpoint(path string) string { return strings.TrimRight(c.BaseURL, "/") + path }

func (c *RemoteClient) Exists(ctx context.Context, hashes []domain.ChunkHash) (map[domain.ChunkHash]bool, error) {
	values := make([]string, len(hashes))
	for i, h := range hashes {
		values[i] = string(h)
	}
	body, err := json.Marshal(protocol.ObjectExistsRequest{SpaceID: c.SpaceID, Hashes: values})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/v1/objects/exists"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, readRemoteError(resp)
	}
	var decoded protocol.ObjectExistsResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	result := make(map[domain.ChunkHash]bool, len(hashes))
	for _, h := range hashes {
		result[h] = decoded.Exists[string(h)]
	}
	return result, nil
}

func (c *RemoteClient) Upload(ctx context.Context, hash domain.ChunkHash, r io.Reader) error {
	if len(hash) != 64 {
		return fmt.Errorf("invalid object hash")
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(domain.DefaultChunkSize)+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > domain.DefaultChunkSize {
		return fmt.Errorf("object exceeds 4 MiB limit")
	}
	if !Verify(data, hash) {
		return fmt.Errorf("object hash mismatch")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/v1/objects/upload?hash=")+string(hash), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	c.auth(req)
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readRemoteError(resp)
	}
	return nil
}

func (c *RemoteClient) UploadBytes(ctx context.Context, data []byte) (domain.ChunkHash, error) {
	hash := Hash(data)
	return hash, c.Upload(ctx, hash, bytes.NewReader(data))
}

func (c *RemoteClient) Download(ctx context.Context, hash domain.ChunkHash) (io.ReadCloser, error) {
	if len(hash) != 64 {
		return nil, fmt.Errorf("invalid object hash")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/v1/objects/")+string(hash), nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, readRemoteError(resp)
	}
	return resp.Body, nil
}

func (c *RemoteClient) auth(req *http.Request) {
	req.Header.Set("X-Device-ID", c.DeviceID)
	if c.DeviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.DeviceToken)
	}
	if c.SpaceID != "" {
		req.Header.Set("X-Space-ID", c.SpaceID)
	}
}

func readRemoteError(resp *http.Response) error {
	var api protocol.APIError
	if json.NewDecoder(resp.Body).Decode(&api) == nil && api.Code != "" {
		return fmt.Errorf("remote %s: %s", api.Code, api.Message)
	}
	return fmt.Errorf("remote object request failed: %s", resp.Status)
}
