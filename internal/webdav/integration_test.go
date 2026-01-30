// Integration tests for WebDAV adapter against a real FileBrowser server.
// Run with: go test -v -tags=integration -run Integration
//
// Required environment variables:
//   FILEBROWSER_URL      - FileBrowser server URL (e.g., http://localhost:8080)
//   FILEBROWSER_USERNAME - Username for authentication
//   FILEBROWSER_PASSWORD - Password for authentication
//
// Example:
//   $env:FILEBROWSER_URL = "http://localhost:8080"
//   $env:FILEBROWSER_USERNAME = "admin"
//   $env:FILEBROWSER_PASSWORD = "admin"
//   go test -v -tags=integration -run Integration ./internal/webdav/

//go:build integration

package webdav

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noiseonwires/filebrowser-webdav-adapter/internal/client"
)

var (
	testFileBrowserURL string
	testUsername       string
	testPassword       string
	testAuthHeader     string
)

func init() {
	testFileBrowserURL = os.Getenv("FILEBROWSER_URL")
	testUsername = os.Getenv("FILEBROWSER_USERNAME")
	testPassword = os.Getenv("FILEBROWSER_PASSWORD")

	if testFileBrowserURL == "" {
		testFileBrowserURL = "http://localhost:8080"
	}
	if testUsername == "" {
		testUsername = "admin"
	}
	if testPassword == "" {
		testPassword = "admin"
	}

	// Create Basic auth header
	testAuthHeader = "Basic " + base64.StdEncoding.EncodeToString(
		[]byte(testUsername+":"+testPassword))
}

func setupIntegrationHandler(t *testing.T) *Handler {
	fbClient := client.New(testFileBrowserURL, client.WithDebug(true))
	return New(fbClient, WithDebug(true))
}

func TestIntegrationConnection(t *testing.T) {
	handler := setupIntegrationHandler(t)

	// Test OPTIONS (no auth required for this)
	req := httptest.NewRequest("OPTIONS", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS failed with status %d", rec.Code)
	}

	t.Logf("DAV header: %s", rec.Header().Get("DAV"))
	t.Logf("Allow header: %s", rec.Header().Get("Allow"))
}

func TestIntegrationAuth(t *testing.T) {
	handler := setupIntegrationHandler(t)

	// Test without auth
	req := httptest.NewRequest("PROPFIND", "/", nil)
	req.Header.Set("Depth", "0")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 without auth, got %d", rec.Code)
	}

	// Test with valid auth
	req = httptest.NewRequest("PROPFIND", "/", nil)
	req.Header.Set("Authorization", testAuthHeader)
	req.Header.Set("Depth", "0")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Errorf("Expected 207 with valid auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIntegrationPropfindRoot(t *testing.T) {
	handler := setupIntegrationHandler(t)

	req := httptest.NewRequest("PROPFIND", "/", nil)
	req.Header.Set("Authorization", testAuthHeader)
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND failed with status %d: %s", rec.Code, rec.Body.String())
	}

	var ms multistatus
	if err := xml.Unmarshal(rec.Body.Bytes(), &ms); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	t.Logf("Found %d items in root:", len(ms.Responses))
	for _, resp := range ms.Responses {
		isDir := resp.Propstat.Prop.ResourceType != nil &&
			resp.Propstat.Prop.ResourceType.Collection != nil
		typeStr := "file"
		if isDir {
			typeStr = "dir"
		}
		t.Logf("  [%s] %s", typeStr, resp.Href)
	}
}

func TestIntegrationCreateAndDeleteFile(t *testing.T) {
	handler := setupIntegrationHandler(t)
	testPath := fmt.Sprintf("/webdav-test-%d.txt", time.Now().UnixNano())
	testContent := "Hello from WebDAV integration test!"

	// Create file
	t.Logf("Creating file: %s", testPath)
	req := httptest.NewRequest("PUT", testPath, strings.NewReader(testContent))
	req.Header.Set("Authorization", testAuthHeader)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("PUT failed with status %d: %s", rec.Code, rec.Body.String())
	}
	t.Logf("File created successfully")

	// Read file back
	t.Logf("Reading file back")
	req = httptest.NewRequest("GET", testPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET failed with status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if body != testContent {
		t.Errorf("Content mismatch: expected %q, got %q", testContent, body)
	}
	t.Logf("Content verified: %s", body)

	// Delete file
	t.Logf("Deleting file")
	req = httptest.NewRequest("DELETE", testPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("DELETE failed with status %d: %s", rec.Code, rec.Body.String())
	}
	t.Logf("File deleted successfully")

	// Verify file is gone
	req = httptest.NewRequest("GET", testPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("Expected 404 after delete, got %d", rec.Code)
	}
}

func TestIntegrationCreateAndDeleteDirectory(t *testing.T) {
	handler := setupIntegrationHandler(t)
	testPath := fmt.Sprintf("/webdav-testdir-%d", time.Now().UnixNano())

	// Create directory
	t.Logf("Creating directory: %s", testPath)
	req := httptest.NewRequest("MKCOL", testPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("MKCOL failed with status %d: %s", rec.Code, rec.Body.String())
	}
	t.Logf("Directory created successfully")

	// Verify directory exists
	req = httptest.NewRequest("PROPFIND", testPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	req.Header.Set("Depth", "0")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND on new dir failed with status %d", rec.Code)
	}

	var ms multistatus
	xml.Unmarshal(rec.Body.Bytes(), &ms)
	if len(ms.Responses) == 0 {
		t.Fatal("No response for directory")
	}
	if ms.Responses[0].Propstat.Prop.ResourceType == nil ||
		ms.Responses[0].Propstat.Prop.ResourceType.Collection == nil {
		t.Error("Directory not marked as collection")
	}

	// Delete directory
	t.Logf("Deleting directory")
	req = httptest.NewRequest("DELETE", testPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("DELETE dir failed with status %d: %s", rec.Code, rec.Body.String())
	}
	t.Logf("Directory deleted successfully")
}

func TestIntegrationCopyAndMove(t *testing.T) {
	handler := setupIntegrationHandler(t)
	timestamp := time.Now().UnixNano()
	srcPath := fmt.Sprintf("/webdav-src-%d.txt", timestamp)
	copyPath := fmt.Sprintf("/webdav-copy-%d.txt", timestamp)
	movePath := fmt.Sprintf("/webdav-moved-%d.txt", timestamp)
	testContent := "Content for copy/move test"

	// Create source file
	t.Logf("Creating source file: %s", srcPath)
	req := httptest.NewRequest("PUT", srcPath, strings.NewReader(testContent))
	req.Header.Set("Authorization", testAuthHeader)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("PUT failed with status %d", rec.Code)
	}

	// Copy file
	t.Logf("Copying to: %s", copyPath)
	req = httptest.NewRequest("COPY", srcPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	req.Header.Set("Destination", "http://localhost"+copyPath)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("COPY failed with status %d: %s", rec.Code, rec.Body.String())
	}
	t.Logf("Copy successful")

	// Verify copy exists
	req = httptest.NewRequest("GET", copyPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET copy failed with status %d", rec.Code)
	}
	if rec.Body.String() != testContent {
		t.Error("Copy content mismatch")
	}

	// Move original file
	t.Logf("Moving to: %s", movePath)
	req = httptest.NewRequest("MOVE", srcPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	req.Header.Set("Destination", "http://localhost"+movePath)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("MOVE failed with status %d: %s", rec.Code, rec.Body.String())
	}
	t.Logf("Move successful")

	// Verify original is gone
	req = httptest.NewRequest("GET", srcPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("Expected 404 for moved file, got %d", rec.Code)
	}

	// Cleanup
	for _, path := range []string{copyPath, movePath} {
		req = httptest.NewRequest("DELETE", path, nil)
		req.Header.Set("Authorization", testAuthHeader)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
	t.Logf("Cleanup complete")
}

func TestIntegrationLargeFile(t *testing.T) {
	handler := setupIntegrationHandler(t)
	testPath := fmt.Sprintf("/webdav-large-%d.bin", time.Now().UnixNano())

	// Create 1MB of test data
	size := 1024 * 1024
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Upload large file
	t.Logf("Uploading %d bytes to %s", size, testPath)
	start := time.Now()
	req := httptest.NewRequest("PUT", testPath, bytes.NewReader(data))
	req.Header.Set("Authorization", testAuthHeader)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("PUT large file failed with status %d", rec.Code)
	}
	t.Logf("Upload completed in %v", time.Since(start))

	// Download and verify
	t.Logf("Downloading file")
	start = time.Now()
	req = httptest.NewRequest("GET", testPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET large file failed with status %d", rec.Code)
	}
	t.Logf("Download completed in %v", time.Since(start))

	downloaded := rec.Body.Bytes()
	if len(downloaded) != size {
		t.Errorf("Size mismatch: expected %d, got %d", size, len(downloaded))
	}
	if !bytes.Equal(downloaded, data) {
		t.Error("Content mismatch in large file")
	}

	// Cleanup
	req = httptest.NewRequest("DELETE", testPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	t.Logf("Cleanup complete")
}

func TestIntegrationLock(t *testing.T) {
	handler := setupIntegrationHandler(t)
	testPath := fmt.Sprintf("/webdav-lock-%d.txt", time.Now().UnixNano())

	// Create file first
	req := httptest.NewRequest("PUT", testPath, strings.NewReader("lock test"))
	req.Header.Set("Authorization", testAuthHeader)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Lock the file
	t.Logf("Locking file: %s", testPath)
	req = httptest.NewRequest("LOCK", testPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK failed with status %d: %s", rec.Code, rec.Body.String())
	}

	lockToken := rec.Header().Get("Lock-Token")
	t.Logf("Got lock token: %s", lockToken)

	if lockToken == "" {
		t.Error("No Lock-Token header in response")
	}

	// Unlock
	t.Logf("Unlocking file")
	req = httptest.NewRequest("UNLOCK", testPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	req.Header.Set("Lock-Token", lockToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("UNLOCK failed with status %d", rec.Code)
	}

	// Cleanup
	req = httptest.NewRequest("DELETE", testPath, nil)
	req.Header.Set("Authorization", testAuthHeader)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
}
