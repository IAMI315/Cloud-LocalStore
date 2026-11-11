package protocol

import (
	"errors"
	"fmt"
	"strings"
)

const Version = "1"

const (
	DeviceActive  = "active"
	DeviceRevoked = "revoked"
)

type ErrorCode string

const (
	ErrInvalidRequest   ErrorCode = "invalid_request"
	ErrUnauthorized     ErrorCode = "unauthorized"
	ErrNotFound         ErrorCode = "not_found"
	ErrConflict         ErrorCode = "conflict"
	ErrMethodNotAllowed ErrorCode = "method_not_allowed"
	ErrUnsupportedMedia ErrorCode = "unsupported_media_type"
	ErrBodyTooLarge     ErrorCode = "body_too_large"
	ErrNotReady         ErrorCode = "not_ready"
	ErrInternal         ErrorCode = "internal"
	ErrVersionConflict  ErrorCode = "version_conflict"
	ErrRevisionTooOld   ErrorCode = "revision_too_old"
	ErrObjectMissing    ErrorCode = "object_missing"
	ErrHashMismatch     ErrorCode = "hash_mismatch"
	ErrRateLimited      ErrorCode = "rate_limited"
)

type APIError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

type HealthResponse struct {
	Status   string `json:"status"`
	Protocol string `json:"protocol"`
}

type ReadyResponse struct {
	Status   string `json:"status"`
	Protocol string `json:"protocol"`
}

type Device struct {
	DeviceID string `json:"device_id"`
	SpaceID  string `json:"space_id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
}

type DeviceRegisterRequest struct {
	SpaceID      string `json:"space_id"`
	DeviceID     string `json:"device_id"`
	Name         string `json:"name"`
	PairingToken string `json:"pairing_token,omitempty"`
}

type DeviceRegisterResponse struct {
	Device      Device `json:"device"`
	DeviceToken string `json:"device_token,omitempty"`
}

type PairingStartResponse struct {
	PairingToken string `json:"pairing_token"`
}

type DeviceRevokeRequest struct {
	SpaceID  string `json:"space_id"`
	DeviceID string `json:"device_id"`
}

type DeviceRevokeResponse struct {
	Device Device `json:"device"`
}

type WorkspaceState struct {
	SpaceID string   `json:"space_id"`
	Devices []Device `json:"devices"`
}

// OperationPrepareRequest contains the immutable metadata for an operation.
// The origin device is authenticated from X-Device-ID; when supplied it must
// match that header.
type OperationPrepareRequest struct {
	OperationID         string `json:"operation_id"`
	OriginDeviceID      string `json:"origin_device_id,omitempty"`
	ParentRevision      int64  `json:"parent_revision"`
	PathID              string `json:"path_id"`
	OperationType       string `json:"operation_type"`
	FileManifestID      string `json:"file_manifest_id,omitempty"`
	BaseFileVersionID   string `json:"base_file_version_id,omitempty"`
	DeviceLocalSequence int64  `json:"device_local_sequence"`
}

type OperationCommitRequest struct {
	OperationID string `json:"operation_id"`
}

type Operation struct {
	OperationID         string `json:"operation_id"`
	SpaceID             string `json:"space_id"`
	OriginDeviceID      string `json:"origin_device_id"`
	ParentRevision      int64  `json:"parent_revision"`
	Revision            int64  `json:"revision"`
	PathID              string `json:"path_id"`
	OperationType       string `json:"operation_type"`
	FileManifestID      string `json:"file_manifest_id,omitempty"`
	BaseFileVersionID   string `json:"base_file_version_id,omitempty"`
	DeviceLocalSequence int64  `json:"device_local_sequence"`
	State               string `json:"state"`
	CreatedAt           string `json:"created_at,omitempty"`
}

type OperationPrepareResponse struct {
	Operation Operation `json:"operation"`
}
type OperationCommitResponse struct {
	Operation Operation `json:"operation"`
}

type OperationListResponse struct {
	SpaceID         string      `json:"space_id"`
	AfterRevision   int64       `json:"after_revision"`
	CurrentRevision int64       `json:"current_revision"`
	Operations      []Operation `json:"operations"`
}

type AckRequest struct {
	AckRevision         int64 `json:"ack_revision"`
	LastSeenRevision    int64 `json:"last_seen_revision,omitempty"`
	LastAppliedRevision int64 `json:"last_applied_revision,omitempty"`
	DeviceEpoch         int64 `json:"device_epoch,omitempty"`
}

type AckResponse struct {
	DeviceID            string `json:"device_id"`
	LastSeenRevision    int64  `json:"last_seen_revision"`
	LastAppliedRevision int64  `json:"last_applied_revision"`
	LastAckRevision     int64  `json:"last_ack_revision"`
	DeviceEpoch         int64  `json:"device_epoch"`
}

// ObjectExistsRequest/Response are intentionally small and stable. Objects
// are addressed by their lowercase BLAKE3-256 hash.
type ObjectExistsRequest struct {
	SpaceID string   `json:"space_id,omitempty"`
	Hashes  []string `json:"hashes"`
}
type ObjectExistsResponse struct {
	Exists  map[string]bool `json:"exists"`
	Present []string        `json:"present,omitempty"`
	Missing []string        `json:"missing,omitempty"`
}
type ObjectUploadResponse struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

const (
	MaxIDLength   = 128
	MaxNameLength = 128
)

func ValidateDeviceRegisterRequest(r DeviceRegisterRequest) error {
	if err := validateID("space_id", r.SpaceID); err != nil {
		return err
	}
	if err := validateID("device_id", r.DeviceID); err != nil {
		return err
	}
	return validateName(r.Name)
}

func ValidatePairingToken(token string) error { return validateID("pairing_token", token) }

func ValidateDeviceRevokeRequest(r DeviceRevokeRequest) error {
	if err := validateID("space_id", r.SpaceID); err != nil {
		return err
	}
	return validateID("device_id", r.DeviceID)
}

func ValidateSpaceID(id string) error { return validateID("space_id", id) }

func ValidateOperationPrepareRequest(r OperationPrepareRequest) error {
	if err := validateID("operation_id", r.OperationID); err != nil {
		return err
	}
	if r.OriginDeviceID != "" {
		if err := validateID("origin_device_id", r.OriginDeviceID); err != nil {
			return err
		}
	}
	if r.ParentRevision < 0 || r.DeviceLocalSequence < 0 {
		return errors.New("revision values must be non-negative")
	}
	if err := validateID("path_id", r.PathID); err != nil {
		return err
	}
	if err := validateID("operation_type", r.OperationType); err != nil {
		return err
	}
	validType := false
	for _, typ := range []string{"create", "update", "delete", "rename", "restore"} {
		if r.OperationType == typ {
			validType = true
			break
		}
	}
	if !validType {
		return errors.New("unsupported operation_type")
	}
	if r.FileManifestID != "" {
		if err := validateID("file_manifest_id", r.FileManifestID); err != nil {
			return err
		}
	}
	if r.BaseFileVersionID != "" {
		if err := validateID("base_file_version_id", r.BaseFileVersionID); err != nil {
			return err
		}
	}
	return nil
}

func ValidateOperationCommitRequest(r OperationCommitRequest) error {
	return validateID("operation_id", r.OperationID)
}

func ValidateAckRequest(r AckRequest) error {
	if r.AckRevision < 0 || r.LastSeenRevision < 0 || r.LastAppliedRevision < 0 || r.DeviceEpoch < 0 {
		return errors.New("revision values must be non-negative")
	}
	return nil
}

func ValidateObjectExistsRequest(r ObjectExistsRequest) error {
	if len(r.Hashes) == 0 || len(r.Hashes) > 1000 {
		return errors.New("hashes must contain between 1 and 1000 values")
	}
	for _, h := range r.Hashes {
		if len(h) != 64 {
			return errors.New("invalid object hash")
		}
		for _, c := range h {
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') && !(c >= 'A' && c <= 'F') {
				return errors.New("invalid object hash")
			}
		}
	}
	return nil
}

func validateID(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len(value) > MaxIDLength {
		return fmt.Errorf("%s is too long", field)
	}
	for _, r := range value {
		if r < 0x21 || r == '/' || r == '\\' || r == '"' {
			return fmt.Errorf("%s contains invalid characters", field)
		}
	}
	return nil
}

func validateName(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("name is required")
	}
	if len(value) > MaxNameLength {
		return errors.New("name is too long")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return errors.New("name contains invalid characters")
		}
	}
	return nil
}
