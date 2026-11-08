package domain

import "errors"

type SpaceID string
type DeviceID string
type OperationID string
type Revision int64
type FileID string
type FileVersionID string
type ManifestID string
type ChunkHash string

const DefaultChunkSize int64 = 4 * 1024 * 1024

type FileType string

const (
	FileTypeRegular FileType = "regular"
	FileTypeDirectory FileType = "directory"
	FileTypeSymlink FileType = "symlink"
)

type OperationType string

const (
	OperationCreate OperationType = "create"
	OperationUpdate OperationType = "update"
	OperationDelete OperationType = "delete"
	OperationRename OperationType = "rename"
	OperationRestore OperationType = "restore"
)

var (
	ErrInvalidPath = errors.New("invalid relative path")
	ErrNotRegular = errors.New("path is not a regular file")
	ErrUnstableFile = errors.New("file changed while being inspected")
)
