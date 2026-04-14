package bootstrap

import (
	"archive/zip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFindAsset(t *testing.T) {
	asset, ok := findAsset([]ReleaseAsset{
		{Name: "sd-master-123-bin-win-cuda12-x64.zip", URL: "https://example.invalid/bin.zip"},
	}, targetPatterns["win-cuda-x64"])
	if !ok {
		t.Fatal("expected a matching asset")
	}
	if asset.Name != "sd-master-123-bin-win-cuda12-x64.zip" {
		t.Fatalf("unexpected asset %q", asset.Name)
	}
}

func TestDownloadToFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("purple-lightswitch"))
	}))
	defer server.Close()

	dst := filepath.Join(t.TempDir(), "download.txt")
	if err := downloadToFile(context.Background(), server.URL, dst, "test-download", "fixture", Reporter{}); err != nil {
		t.Fatalf("downloadToFile failed: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(data) != "purple-lightswitch" {
		t.Fatalf("unexpected payload %q", string(data))
	}
}

func TestExtractZipRejectsTraversal(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "unsafe.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	archive := zip.NewWriter(file)
	entry, err := archive.Create("../escape.txt")
	if err != nil {
		t.Fatalf("Create entry failed: %v", err)
	}
	if _, err := entry.Write([]byte("nope")); err != nil {
		t.Fatalf("Write entry failed: %v", err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("Close archive failed: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close file failed: %v", err)
	}

	err = extractZip(zipPath, filepath.Join(t.TempDir(), "out"), "extract", "fixture", Reporter{})
	if err == nil {
		t.Fatal("expected extractZip to reject path traversal")
	}
}
