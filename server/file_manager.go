package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StoredFile holds metadata about an uploaded or received file
type StoredFile struct {
	ID          string    `json:"id"`
	Filename    string    `json:"filename"`
	Size        int64     `json:"size"`
	UploadTime  time.Time `json:"upload_time"`
	SourceNode  string    `json:"source_node,omitempty"`
	TargetNode  string    `json:"target_node,omitempty"`
	ContentType string    `json:"content_type"`
}

// FileManager manages temporary file storage for bidirectional transfers
type FileManager struct {
	mu      sync.RWMutex
	baseDir string
	files   map[string]StoredFile
}

// NewFileManager creates a storage directory and initializes the manager
func NewFileManager(baseDir string) (*FileManager, error) {
	if baseDir == "" {
		baseDir = "mar4uder_files"
	}

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create file storage dir %s: %w", baseDir, err)
	}

	return &FileManager{
		baseDir: baseDir,
		files:   make(map[string]StoredFile),
	}, nil
}

func generateFileID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SaveFile streams an incoming file to disk and records metadata
func (fm *FileManager) SaveFile(filename string, r io.Reader, sourceNode, targetNode string) (*StoredFile, error) {
	fileID := generateFileID()
	safeFilename := filepath.Base(filename)
	if safeFilename == "" || safeFilename == "." || safeFilename == "/" {
		safeFilename = "file_" + fileID
	}

	destPath := filepath.Join(fm.baseDir, fileID+"_"+safeFilename)
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to create destination file: %w", err)
	}
	defer out.Close()

	n, err := io.Copy(out, r)
	if err != nil {
		_ = os.Remove(destPath)
		return nil, fmt.Errorf("failed writing file content: %w", err)
	}

	sf := StoredFile{
		ID:         fileID,
		Filename:   safeFilename,
		Size:       n,
		UploadTime: time.Now(),
		SourceNode: sourceNode,
		TargetNode: targetNode,
	}

	fm.mu.Lock()
	fm.files[fileID] = sf
	fm.mu.Unlock()

	return &sf, nil
}

// GetFile retrieves metadata and disk path for a stored file
func (fm *FileManager) GetFile(id string) (StoredFile, string, error) {
	fm.mu.RLock()
	sf, ok := fm.files[id]
	fm.mu.RUnlock()

	if !ok {
		return StoredFile{}, "", fmt.Errorf("file '%s' not found", id)
	}

	destPath := filepath.Join(fm.baseDir, sf.ID+"_"+sf.Filename)
	if _, err := os.Stat(destPath); err != nil {
		return StoredFile{}, "", fmt.Errorf("file data missing on disk: %w", err)
	}

	return sf, destPath, nil
}

// ListFiles returns all stored files
func (fm *FileManager) ListFiles() []StoredFile {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	list := make([]StoredFile, 0, len(fm.files))
	for _, f := range fm.files {
		list = append(list, f)
	}
	return list
}

// DeleteFile removes a file from disk and manager
func (fm *FileManager) DeleteFile(id string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	sf, ok := fm.files[id]
	if !ok {
		return nil
	}

	destPath := filepath.Join(fm.baseDir, sf.ID+"_"+sf.Filename)
	_ = os.Remove(destPath)
	delete(fm.files, id)
	return nil
}
