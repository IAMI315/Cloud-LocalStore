package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	"github.com/IAMI315/Cloud-LocalStore/internal/protocol"
	"github.com/IAMI315/Cloud-LocalStore/internal/store"
)

const maxBodySize = 1 << 20
const maxObjectSize = int64(4 * 1024 * 1024)

type Service struct {
	app             *App
	events          *deviceEventHub
	statusMu        sync.Mutex
	lastConnections map[string]string
}

func NewService(app *App) *Service {
	s := &Service{app: app, events: newDeviceEventHub(), lastConnections: make(map[string]string)}
	go s.deviceStatusLoop()
	return s
}

type deviceEvent struct {
	id    uint64
	value protocol.DeviceEvent
}

type deviceEventHub struct {
	mu          sync.Mutex
	nextID      atomic.Uint64
	subscribers map[string]map[chan deviceEvent]struct{}
}

func newDeviceEventHub() *deviceEventHub {
	return &deviceEventHub{subscribers: make(map[string]map[chan deviceEvent]struct{})}
}

func (h *deviceEventHub) subscribe(space string) (chan deviceEvent, func()) {
	ch := make(chan deviceEvent, 16)
	h.mu.Lock()
	if h.subscribers[space] == nil {
		h.subscribers[space] = make(map[chan deviceEvent]struct{})
	}
	h.subscribers[space][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if peers := h.subscribers[space]; peers != nil {
			delete(peers, ch)
			if len(peers) == 0 {
				delete(h.subscribers, space)
			}
		}
		h.mu.Unlock()
	}
}

func (h *deviceEventHub) publish(value protocol.DeviceEvent) {
	e := deviceEvent{id: h.nextID.Add(1), value: value}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers[value.SpaceID] {
		select {
		case ch <- e:
		default:
			// A stale client will reconcile from /state after reconnect. Do not
			// let one slow stream block the service or other subscribers.
		}
	}
}

func (h *deviceEventHub) spaces() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]string, 0, len(h.subscribers))
	for space := range h.subscribers {
		result = append(result, space)
	}
	return result
}

// deviceStatusLoop is the server-side lease check. Presence heartbeats keep
// last_seen_at current; this loop turns a missing heartbeat into an idle or
// offline event for any connected SSE clients without requiring a request
// from the disappeared device.
func (s *Service) deviceStatusLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		for _, space := range s.events.spaces() {
			devices, err := s.app.Store().WorkspaceState(context.Background(), space)
			if err != nil {
				continue
			}
			for _, device := range devices {
				status := connectionStatus(device)
				key := space + "\x00" + device.DeviceID
				s.statusMu.Lock()
				previous, known := s.lastConnections[key]
				s.lastConnections[key] = status
				s.statusMu.Unlock()
				if known && previous != status {
					s.publishDevice(space, device)
				}
			}
		}
	}
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/readyz", s.ready)
	mux.HandleFunc("/v1/devices/register", s.register)
	mux.HandleFunc("/v1/pairing/start", s.pairingStart)
	mux.HandleFunc("/v1/devices/revoke", s.revoke)
	mux.HandleFunc("/v1/objects/exists", s.objectExists)
	mux.HandleFunc("/v1/objects/upload", s.objectUpload)
	mux.HandleFunc("/v1/objects/", s.objectDownload)
	mux.HandleFunc("/v1/spaces/", s.state)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func (s *Service) objectAuthorized(r *http.Request) bool {
	id := strings.TrimSpace(r.Header.Get("X-Device-ID"))
	if id == "" {
		return false
	}
	d, err := s.authenticatedDevice(r)
	if err != nil || d.Status != storeDeviceActive {
		return false
	}
	space := strings.TrimSpace(r.Header.Get("X-Space-ID"))
	return space == "" || d.SpaceID == space
}

func (s *Service) authenticatedDevice(r *http.Request) (store.Device, error) {
	id := strings.TrimSpace(r.Header.Get("X-Device-ID"))
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	var (
		d   store.Device
		err error
	)
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		d, err = s.app.Store().AuthenticateDevice(r.Context(), id, strings.TrimSpace(auth[7:]))
	} else if s.app.Config().DevMode {
		d, err = s.app.Store().GetActiveDevice(r.Context(), id)
	} else {
		return store.Device{}, store.ErrNotFound
	}
	if err == nil && d.Status == storeDeviceActive {
		before, beforeErr := s.app.Store().GetDevicePresence(r.Context(), d.SpaceID, d.DeviceID)
		_ = s.app.Store().TouchDevice(r.Context(), d.SpaceID, d.DeviceID)
		if after, afterErr := s.app.Store().GetDevicePresence(r.Context(), d.SpaceID, d.DeviceID); afterErr == nil {
			d = after
			if beforeErr != nil || connectionStatus(before) != connectionStatus(after) {
				s.publishDevice(after.SpaceID, after)
			}
		}
	}
	return d, err
}

func (s *Service) pairingStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.error(w, protocol.ErrMethodNotAllowed, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.app.Config().DevMode {
		s.error(w, protocol.ErrNotFound, "pairing endpoint disabled", http.StatusNotFound)
		return
	}
	s.json(w, http.StatusOK, protocol.PairingStartResponse{PairingToken: s.app.PairingToken()})
}

func validObjectHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (s *Service) objectExists(w http.ResponseWriter, r *http.Request) {
	if !s.requireJSON(w, r, http.MethodPost) {
		return
	}
	if !s.objectAuthorized(r) {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	var req protocol.ObjectExistsRequest
	if !s.decode(w, r, &req) {
		return
	}
	if err := protocol.ValidateObjectExistsRequest(req); err != nil {
		s.error(w, protocol.ErrInvalidRequest, "invalid object request", http.StatusBadRequest)
		return
	}
	result := protocol.ObjectExistsResponse{Exists: make(map[string]bool, len(req.Hashes))}
	for _, value := range req.Hashes {
		hash := domain.ChunkHash(strings.ToLower(value))
		ok, err := s.app.Store().Objects.Has(r.Context(), hash)
		if err != nil {
			s.error(w, protocol.ErrInternal, "object lookup failed", http.StatusInternalServerError)
			return
		}
		result.Exists[value] = ok
		if ok {
			result.Present = append(result.Present, value)
		} else {
			result.Missing = append(result.Missing, value)
		}
	}
	s.json(w, http.StatusOK, result)
}

func (s *Service) objectUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		s.error(w, protocol.ErrMethodNotAllowed, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.objectAuthorized(r) {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	hash := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("hash")))
	if hash == "" {
		hash = strings.ToLower(strings.TrimSpace(r.Header.Get("X-Object-Hash")))
	}
	if !validObjectHash(hash) {
		s.error(w, protocol.ErrInvalidRequest, "object hash is required", http.StatusBadRequest)
		return
	}
	if r.ContentLength > maxObjectSize {
		s.error(w, protocol.ErrBodyTooLarge, "object is too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxObjectSize)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "request body too large") {
			s.error(w, protocol.ErrBodyTooLarge, "object is too large", http.StatusRequestEntityTooLarge)
		} else {
			s.error(w, protocol.ErrInvalidRequest, "invalid object body", http.StatusBadRequest)
		}
		return
	}
	if !objectstore.Verify(b, domain.ChunkHash(hash)) {
		s.error(w, protocol.ErrHashMismatch, "object hash mismatch", http.StatusBadRequest)
		return
	}
	if err := s.app.Store().Objects.Put(r.Context(), objectstore.Chunk{Hash: domain.ChunkHash(hash), Size: int64(len(b)), Data: b}); err != nil {
		s.error(w, protocol.ErrInternal, "object upload failed", http.StatusInternalServerError)
		return
	}
	s.json(w, http.StatusOK, protocol.ObjectUploadResponse{Hash: hash, Size: int64(len(b))})
}

func (s *Service) objectDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.error(w, protocol.ErrMethodNotAllowed, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.objectAuthorized(r) {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	hash := strings.TrimPrefix(r.URL.Path, "/v1/objects/")
	if strings.Contains(hash, "/") || !validObjectHash(hash) {
		s.error(w, protocol.ErrInvalidRequest, "invalid object hash", http.StatusBadRequest)
		return
	}
	f, err := s.app.Store().Objects.Open(r.Context(), domain.ChunkHash(strings.ToLower(hash)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.error(w, protocol.ErrNotFound, "object not found", http.StatusNotFound)
		} else {
			s.error(w, protocol.ErrInternal, "object read failed", http.StatusInternalServerError)
		}
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, io.LimitReader(f, maxObjectSize))
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.error(w, protocol.ErrMethodNotAllowed, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.json(w, http.StatusOK, protocol.HealthResponse{Status: "ok", Protocol: protocol.Version})
}

func (s *Service) ready(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.error(w, protocol.ErrMethodNotAllowed, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.app.Ready(ctx); err != nil {
		s.error(w, protocol.ErrNotReady, "server is not ready", http.StatusServiceUnavailable)
		return
	}
	s.json(w, http.StatusOK, protocol.ReadyResponse{Status: "ready", Protocol: protocol.Version})
}

func (s *Service) register(w http.ResponseWriter, r *http.Request) {
	if !s.requireJSON(w, r, http.MethodPost) {
		return
	}
	var req protocol.DeviceRegisterRequest
	if !s.decode(w, r, &req) {
		return
	}
	if err := protocol.ValidateDeviceRegisterRequest(req); err != nil {
		s.error(w, protocol.ErrInvalidRequest, "invalid request", http.StatusBadRequest)
		return
	}
	if !s.app.Config().DevMode && !s.app.VerifyPairingToken(req.PairingToken) {
		s.error(w, protocol.ErrUnauthorized, "valid pairing token required", http.StatusUnauthorized)
		return
	}
	var d store.Device
	var token string
	var err error
	if req.PreviousDeviceID != "" {
		d, token, err = s.app.Store().RenameDeviceWithToken(r.Context(), req.SpaceID, req.PreviousDeviceID, req.Name)
	} else {
		d, token, err = s.app.Store().RegisterDeviceWithToken(r.Context(), req.SpaceID, req.DeviceID, req.Name)
	}
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.json(w, http.StatusOK, protocol.DeviceRegisterResponse{Device: protocol.Device{DeviceID: d.DeviceID, SpaceID: d.SpaceID, Name: d.Name, Status: d.Status}, DeviceToken: token})
}

func (s *Service) revoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireJSON(w, r, http.MethodPost) {
		return
	}
	var req protocol.DeviceRevokeRequest
	if !s.decode(w, r, &req) {
		return
	}
	if err := protocol.ValidateDeviceRevokeRequest(req); err != nil {
		s.error(w, protocol.ErrInvalidRequest, "invalid request", http.StatusBadRequest)
		return
	}
	if !s.authorized(r, req.SpaceID) {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	d, err := s.app.Store().RevokeDevice(r.Context(), req.SpaceID, req.DeviceID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if presence, presenceErr := s.app.Store().GetDevicePresence(r.Context(), req.SpaceID, req.DeviceID); presenceErr == nil {
		s.publishDevice(req.SpaceID, presence)
	}
	s.json(w, http.StatusOK, protocol.DeviceRevokeResponse{Device: protocol.Device{DeviceID: d.DeviceID, SpaceID: d.SpaceID, Name: d.Name, Status: d.Status}})
}

func (s *Service) state(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/spaces/")
	if strings.HasSuffix(path, "/events") {
		s.eventsStream(w, r)
		return
	}
	if strings.HasSuffix(path, "/presence") {
		s.presence(w, r)
		return
	}
	if strings.HasSuffix(path, "/operations/prepare") {
		s.prepareOperation(w, r)
		return
	}
	if strings.HasSuffix(path, "/operations/commit") {
		s.commitOperation(w, r)
		return
	}
	if strings.HasSuffix(path, "/operations") {
		s.listOperations(w, r)
		return
	}
	if strings.HasSuffix(path, "/ack") {
		s.ack(w, r)
		return
	}
	if strings.HasSuffix(path, "/quota") {
		s.quota(w, r)
		return
	}
	if r.Method != http.MethodGet {
		s.error(w, protocol.ErrMethodNotAllowed, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	space := strings.TrimPrefix(r.URL.Path, "/v1/spaces/")
	space = strings.TrimSuffix(space, "/state")
	if strings.Contains(space, "/") || protocol.ValidateSpaceID(space) != nil {
		s.error(w, protocol.ErrInvalidRequest, "invalid workspace", http.StatusBadRequest)
		return
	}
	if !s.authorized(r, space) {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	devices, err := s.app.Store().WorkspaceState(r.Context(), space)
	if err != nil {
		s.storeError(w, err)
		return
	}
	result := protocol.WorkspaceState{SpaceID: space, ServerTime: time.Now().Format(time.RFC3339), Devices: make([]protocol.Device, 0, len(devices))}
	result.CurrentRevision, _ = s.app.Store().CurrentRevision(r.Context(), space)
	result.QuotaBytes, result.UsedBytes, err = s.app.Store().SpaceQuota(r.Context(), space)
	if err != nil {
		s.storeError(w, err)
		return
	}
	for _, d := range devices {
		result.Devices = append(result.Devices, s.protocolDevice(d))
	}
	s.json(w, http.StatusOK, result)
}

func (s *Service) quota(w http.ResponseWriter, r *http.Request) {
	space := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/spaces/"), "/quota")
	if strings.Contains(space, "/") || protocol.ValidateSpaceID(space) != nil {
		s.error(w, protocol.ErrInvalidRequest, "invalid workspace", http.StatusBadRequest)
		return
	}
	if !s.authorized(r, space) {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		quota, used, err := s.app.Store().SpaceQuota(r.Context(), space)
		if err != nil {
			s.storeError(w, err)
			return
		}
		s.json(w, http.StatusOK, protocol.SetQuotaResponse{SpaceID: space, QuotaBytes: quota, UsedBytes: used})
	case http.MethodPut:
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			s.error(w, protocol.ErrUnsupportedMedia, "content type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		var req protocol.SetQuotaRequest
		if !s.decode(w, r, &req) {
			return
		}
		if err := protocol.ValidateSetQuotaRequest(req); err != nil {
			s.error(w, protocol.ErrInvalidRequest, "invalid quota", http.StatusBadRequest)
			return
		}
		quota, used, err := s.app.Store().SetSpaceQuota(r.Context(), space, req.QuotaBytes)
		if err != nil {
			s.storeError(w, err)
			return
		}
		s.json(w, http.StatusOK, protocol.SetQuotaResponse{SpaceID: space, QuotaBytes: quota, UsedBytes: used})
	default:
		s.error(w, protocol.ErrMethodNotAllowed, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Service) presence(w http.ResponseWriter, r *http.Request) {
	if !s.requireJSON(w, r, http.MethodPost) {
		return
	}
	space := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/spaces/"), "/presence")
	if strings.Contains(space, "/") || protocol.ValidateSpaceID(space) != nil {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	device, err := s.authenticatedDevice(r)
	if err != nil || device.Status != storeDeviceActive || device.SpaceID != space {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	var req protocol.PresenceRequest
	if !s.decode(w, r, &req) {
		return
	}
	deviceID := strings.TrimSpace(r.Header.Get("X-Device-ID"))
	before, beforeErr := s.app.Store().GetDevicePresence(r.Context(), space, deviceID)
	d, err := s.app.Store().UpdatePresence(r.Context(), space, deviceID, req.ClientStatus, req.Monitoring, req.SyncCompleted)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if beforeErr != nil || before.ClientStatus != d.ClientStatus || before.Monitoring != d.Monitoring || before.LastSyncAt != d.LastSyncAt || connectionStatus(before) != connectionStatus(d) {
		s.publishDevice(space, d)
	}
	s.json(w, http.StatusOK, protocol.PresenceResponse{Device: s.protocolDevice(d)})
}

func (s *Service) eventsStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.error(w, protocol.ErrMethodNotAllowed, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	space := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/spaces/"), "/events")
	if strings.Contains(space, "/") || protocol.ValidateSpaceID(space) != nil {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	device, err := s.authenticatedDevice(r)
	if err != nil || device.Status != storeDeviceActive || device.SpaceID != space {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.error(w, protocol.ErrInternal, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsubscribe := s.events.subscribe(space)
	defer unsubscribe()
	defer func() {
		if disconnected, disconnectErr := s.app.Store().MarkDeviceDisconnected(context.Background(), space, device.DeviceID); disconnectErr == nil {
			s.publishDevice(space, disconnected)
		}
	}()
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-ch:
			body, err := json.Marshal(event.value)
			if err != nil {
				return
			}
			if _, err = io.WriteString(w, "id: "+strconv.FormatUint(event.id, 10)+"\nevent: device.status.changed\ndata: "+string(body)+"\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Service) publishDevice(space string, d store.Device) {
	s.events.publish(protocol.DeviceEvent{SpaceID: space, Device: s.protocolDevice(d)})
}

func (s *Service) protocolDevice(d store.Device) protocol.Device {
	return protocol.Device{DeviceID: d.DeviceID, SpaceID: d.SpaceID, Name: d.Name, Status: d.Status, ConnectionStatus: connectionStatus(d), ClientStatus: d.ClientStatus, Monitoring: d.Monitoring, LastSeenAt: serverProtocolTime(d.LastSeenAt), LastStatusAt: serverProtocolTime(d.LastStatusAt), LastSyncAt: serverProtocolTime(d.LastSyncAt), LastSeenRevision: d.LastSeenRevision, LastAppliedRevision: d.LastAppliedRevision, LastAckRevision: d.LastAckRevision, DeviceEpoch: d.DeviceEpoch}
}

func connectionStatus(d store.Device) string {
	if d.Status != storeDeviceActive {
		return "revoked"
	}
	if d.LastSeenAt == "" {
		return "offline"
	}
	seen, err := parseServerTime(d.LastSeenAt)
	if err != nil {
		return "offline"
	}
	age := time.Since(seen)
	switch {
	case age <= 45*time.Second:
		return "online"
	case age <= 2*time.Minute:
		return "idle"
	default:
		return "offline"
	}
}

func spaceFromPath(path, suffix string) string {
	space := strings.TrimPrefix(path, "/v1/spaces/")
	return strings.TrimSuffix(space, suffix)
}

func (s *Service) prepareOperation(w http.ResponseWriter, r *http.Request) {
	if !s.requireJSON(w, r, http.MethodPost) {
		return
	}
	space := spaceFromPath(r.URL.Path, "/operations/prepare")
	if strings.Contains(space, "/") || protocol.ValidateSpaceID(space) != nil {
		s.error(w, protocol.ErrInvalidRequest, "invalid workspace", http.StatusBadRequest)
		return
	}
	if !s.authorized(r, space) {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	var req protocol.OperationPrepareRequest
	if !s.decode(w, r, &req) {
		return
	}
	if err := protocol.ValidateOperationPrepareRequest(req); err != nil {
		s.error(w, protocol.ErrInvalidRequest, "invalid request", http.StatusBadRequest)
		return
	}
	deviceID := strings.TrimSpace(r.Header.Get("X-Device-ID"))
	if req.OriginDeviceID != "" && req.OriginDeviceID != deviceID {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	op, err := s.app.Store().PrepareOperation(r.Context(), space, deviceID, store.Operation{OperationID: req.OperationID, OriginDeviceID: deviceID, ParentRevision: req.ParentRevision, PathID: req.PathID, OperationType: req.OperationType, FileManifestID: req.FileManifestID, BaseFileVersionID: req.BaseFileVersionID, DeviceLocalSequence: req.DeviceLocalSequence})
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.json(w, http.StatusOK, protocol.OperationPrepareResponse{Operation: operationResponse(op)})
}

func (s *Service) commitOperation(w http.ResponseWriter, r *http.Request) {
	if !s.requireJSON(w, r, http.MethodPost) {
		return
	}
	space := spaceFromPath(r.URL.Path, "/operations/commit")
	if strings.Contains(space, "/") || protocol.ValidateSpaceID(space) != nil {
		s.error(w, protocol.ErrInvalidRequest, "invalid workspace", http.StatusBadRequest)
		return
	}
	if !s.authorized(r, space) {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	var req protocol.OperationCommitRequest
	if !s.decode(w, r, &req) {
		return
	}
	if err := protocol.ValidateOperationCommitRequest(req); err != nil {
		s.error(w, protocol.ErrInvalidRequest, "invalid request", http.StatusBadRequest)
		return
	}
	op, err := s.app.Store().CommitOperation(r.Context(), space, strings.TrimSpace(r.Header.Get("X-Device-ID")), req.OperationID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.json(w, http.StatusOK, protocol.OperationCommitResponse{Operation: operationResponse(op)})
}

func (s *Service) listOperations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.error(w, protocol.ErrMethodNotAllowed, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	space := spaceFromPath(r.URL.Path, "/operations")
	if strings.Contains(space, "/") || protocol.ValidateSpaceID(space) != nil {
		s.error(w, protocol.ErrInvalidRequest, "invalid workspace", http.StatusBadRequest)
		return
	}
	if !s.authorized(r, space) {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	if exists, err := s.app.Store().WorkspaceExists(r.Context(), space); err != nil {
		s.storeError(w, err)
		return
	} else if !exists {
		s.storeError(w, store.ErrNotFound)
		return
	}
	after := int64(0)
	if value := r.URL.Query().Get("after_revision"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			s.error(w, protocol.ErrInvalidRequest, "invalid after_revision", http.StatusBadRequest)
			return
		}
		after = parsed
	}
	ops, current, err := s.app.Store().ListOperations(r.Context(), space, after)
	if err != nil {
		s.storeError(w, err)
		return
	}
	result := protocol.OperationListResponse{SpaceID: space, AfterRevision: after, CurrentRevision: current, Operations: make([]protocol.Operation, 0, len(ops))}
	for _, op := range ops {
		result.Operations = append(result.Operations, operationResponse(op))
	}
	s.json(w, http.StatusOK, result)
}

func (s *Service) ack(w http.ResponseWriter, r *http.Request) {
	if !s.requireJSON(w, r, http.MethodPost) {
		return
	}
	space := spaceFromPath(r.URL.Path, "/ack")
	if strings.Contains(space, "/") || protocol.ValidateSpaceID(space) != nil {
		s.error(w, protocol.ErrInvalidRequest, "invalid workspace", http.StatusBadRequest)
		return
	}
	if !s.authorized(r, space) {
		s.error(w, protocol.ErrUnauthorized, "active device authorization required", http.StatusUnauthorized)
		return
	}
	var req protocol.AckRequest
	if !s.decode(w, r, &req) {
		return
	}
	if err := protocol.ValidateAckRequest(req); err != nil {
		s.error(w, protocol.ErrInvalidRequest, "invalid request", http.StatusBadRequest)
		return
	}
	d, err := s.app.Store().Ack(r.Context(), space, strings.TrimSpace(r.Header.Get("X-Device-ID")), req.LastSeenRevision, req.LastAppliedRevision, req.AckRevision, req.DeviceEpoch)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.json(w, http.StatusOK, protocol.AckResponse{DeviceID: d.DeviceID, LastSeenRevision: d.LastSeenRevision, LastAppliedRevision: d.LastAppliedRevision, LastAckRevision: d.LastAckRevision, DeviceEpoch: d.DeviceEpoch})
}

func operationResponse(op store.Operation) protocol.Operation {
	return protocol.Operation{OperationID: op.OperationID, SpaceID: op.SpaceID, OriginDeviceID: op.OriginDeviceID, ParentRevision: op.ParentRevision, Revision: op.Revision, PathID: op.PathID, OperationType: op.OperationType, FileManifestID: op.FileManifestID, BaseFileVersionID: op.BaseFileVersionID, DeviceLocalSequence: op.DeviceLocalSequence, SizeBytes: op.SizeBytes, State: op.State, CreatedAt: serverProtocolTime(op.CreatedAt)}
}

func (s *Service) authorized(r *http.Request, space string) bool {
	id := strings.TrimSpace(r.Header.Get("X-Device-ID"))
	if id == "" {
		return false
	}
	d, err := s.authenticatedDevice(r)
	if err != nil || d.Status != storeDeviceActive {
		return false
	}
	exists, err := s.app.Store().WorkspaceExists(r.Context(), space)
	if err != nil {
		return false
	}
	// For an unknown workspace, an active device is authenticated but the
	// subsequent lookup still returns not_found. Existing workspaces require
	// membership in that workspace.
	return !exists || d.SpaceID == space
}

const storeDeviceActive = "active"

func (s *Service) requireJSON(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		s.error(w, protocol.ErrMethodNotAllowed, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		s.error(w, protocol.ErrUnsupportedMedia, "content type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

func (s *Service) decode(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.error(w, protocol.ErrBodyTooLarge, "request body is too large", http.StatusRequestEntityTooLarge)
			return false
		}
		s.error(w, protocol.ErrInvalidRequest, "invalid JSON request", http.StatusBadRequest)
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		s.error(w, protocol.ErrInvalidRequest, "request must contain one JSON object", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Service) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.error(w, protocol.ErrNotFound, "workspace or device not found", http.StatusNotFound)
	case errors.Is(err, store.ErrRevoked):
		s.error(w, protocol.ErrConflict, "device is revoked", http.StatusConflict)
	case errors.Is(err, store.ErrConflict):
		s.error(w, protocol.ErrConflict, "device belongs to another workspace", http.StatusConflict)
	case errors.Is(err, store.ErrVersionConflict):
		s.error(w, protocol.ErrVersionConflict, "parent revision does not match current revision", http.StatusConflict)
	case errors.Is(err, store.ErrRevisionTooOld):
		s.error(w, protocol.ErrRevisionTooOld, "revision is too old", http.StatusConflict)
	case errors.Is(err, store.ErrObjectMissing):
		s.error(w, protocol.ErrObjectMissing, "manifest or referenced object is missing", http.StatusConflict)
	case errors.Is(err, store.ErrQuotaExceeded):
		s.error(w, protocol.ErrQuotaExceeded, "workspace storage quota exceeded", http.StatusInsufficientStorage)
	case errors.Is(err, store.ErrQuotaBelowUsed):
		s.error(w, protocol.ErrInvalidRequest, "quota cannot be lower than used bytes", http.StatusBadRequest)
	default:
		s.error(w, protocol.ErrInternal, "internal server error", http.StatusInternalServerError)
	}
}

func (s *Service) json(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Service) error(w http.ResponseWriter, code protocol.ErrorCode, message string, status int) {
	s.json(w, status, protocol.APIError{Code: code, Message: message})
}
