package webdav

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noiseonwires/filebrowser-webdav-adapter/internal/client"
)

// Test XML structures that handle namespaced elements
type testMultistatus struct {
	XMLName   xml.Name               `xml:"multistatus"`
	Responses []testPropfindResponse `xml:"response"`
}

type testPropfindResponse struct {
	Href     string         `xml:"href"`
	Propstat []testPropstat `xml:"propstat"`
}

type testPropstat struct {
	Status string   `xml:"status"`
	Prop   testProp `xml:"prop"`
}

type testProp struct {
	DisplayName      string            `xml:"displayname"`
	ResourceType     testResourceType  `xml:"resourcetype"`
	GetContentLength string            `xml:"getcontentlength"`
	GetContentType   string            `xml:"getcontenttype"`
	GetLastModified  string            `xml:"getlastmodified"`
}

type testResourceType struct {
	Collection *struct{} `xml:"collection"`
}

// mockFileBrowser creates a mock FileBrowser server for testing
func mockFileBrowser(t *testing.T) *httptest.Server {
	files := map[string]*client.FileInfo{
		"/": {
			Path:     "/",
			Name:     "",
			IsDir:    true,
			Modified: time.Now(),
			Items: []client.FileInfo{
				{Path: "/documents", Name: "documents", IsDir: true, Modified: time.Now()},
				{Path: "/test.txt", Name: "test.txt", Size: 13, IsDir: false, Type: "text", Modified: time.Now()},
			},
		},
		"/documents": {
			Path:     "/documents",
			Name:     "documents",
			IsDir:    true,
			Modified: time.Now(),
			Items: []client.FileInfo{
				{Path: "/documents/file1.txt", Name: "file1.txt", Size: 100, IsDir: false, Type: "text", Modified: time.Now()},
			},
		},
		"/test.txt": {
			Path:     "/test.txt",
			Name:     "test.txt",
			Size:     13,
			IsDir:    false,
			Type:     "text",
			Modified: time.Now(),
		},
		"/documents/file1.txt": {
			Path:     "/documents/file1.txt",
			Name:     "file1.txt",
			Size:     100,
			IsDir:    false,
			Type:     "text",
			Modified: time.Now(),
		},
	}

	fileContents := map[string]string{
		"/test.txt":            "Hello, World!",
		"/documents/file1.txt": strings.Repeat("x", 100),
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Login endpoint
		if r.URL.Path == "/api/login" {
			var payload struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			json.NewDecoder(r.Body).Decode(&payload)
			if payload.Username == "admin" && payload.Password == "password" {
				w.Write([]byte("test-token"))
			} else {
				w.WriteHeader(http.StatusForbidden)
			}
			return
		}

		// Check auth
		if r.Header.Get("X-Auth") != "test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Resources endpoint
		if strings.HasPrefix(r.URL.Path, "/api/resources") {
			path := strings.TrimPrefix(r.URL.Path, "/api/resources")
			if path == "" {
				path = "/"
			}
			path = strings.TrimSuffix(path, "/")
			if path == "" {
				path = "/"
			}

			switch r.Method {
			case http.MethodGet:
				if info, ok := files[path]; ok {
					json.NewEncoder(w).Encode(info)
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			case http.MethodPost:
				// Create file or directory
				w.WriteHeader(http.StatusOK)
			case http.MethodPut:
				// Update file
				w.WriteHeader(http.StatusOK)
			case http.MethodDelete:
				w.WriteHeader(http.StatusOK)
			case http.MethodPatch:
				// Copy/Move
				w.WriteHeader(http.StatusOK)
			}
			return
		}

		// Raw download endpoint
		if strings.HasPrefix(r.URL.Path, "/api/raw") {
			path := strings.TrimPrefix(r.URL.Path, "/api/raw")
			if content, ok := fileContents[path]; ok {
				w.Header().Set("Content-Length", strconv.Itoa(len(content)))
				w.Write([]byte(content))
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
}

func TestPropfindRoot(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("PROPFIND", "/", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=") // admin:password
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Errorf("Expected status %d, got %d", http.StatusMultiStatus, rec.Code)
	}

	// Parse response
	var ms testMultistatus
	if err := xml.Unmarshal(rec.Body.Bytes(), &ms); err != nil {
		t.Fatalf("Failed to parse response: %v\nBody: %s", err, rec.Body.String())
	}

	// Should have 3 responses: root, documents folder, test.txt
	if len(ms.Responses) != 3 {
		t.Errorf("Expected 3 responses, got %d", len(ms.Responses))
	}
}

func TestPropfindDepthZero(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("PROPFIND", "/", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	req.Header.Set("Depth", "0")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var ms testMultistatus
	if err := xml.Unmarshal(rec.Body.Bytes(), &ms); err != nil {
		t.Fatalf("Failed to parse response: %v\nBody: %s", err, rec.Body.String())
	}

	// Should have only 1 response (root itself)
	if len(ms.Responses) != 1 {
		t.Errorf("Expected 1 response, got %d", len(ms.Responses))
	}
}

func TestGetFile(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("GET", "/test.txt", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rec.Code)
	}

	body := rec.Body.String()
	if body != "Hello, World!" {
		t.Errorf("Expected body 'Hello, World!', got '%s'", body)
	}
}

func TestGetDirectory(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("GET", "/documents", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rec.Code)
	}

	// Should return HTML directory listing
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("Expected HTML content type, got %s", rec.Header().Get("Content-Type"))
	}
}

func TestPutFile(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	content := "New file content"
	req := httptest.NewRequest("PUT", "/newfile.txt", strings.NewReader(content))
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Should return 201 Created for new file
	if rec.Code != http.StatusCreated {
		t.Errorf("Expected status %d, got %d", http.StatusCreated, rec.Code)
	}
}

func TestMkcol(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("MKCOL", "/newfolder", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("Expected status %d, got %d", http.StatusCreated, rec.Code)
	}
}

func TestDelete(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("DELETE", "/test.txt", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("Expected status %d, got %d", http.StatusNoContent, rec.Code)
	}
}

func TestCopy(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("COPY", "/test.txt", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	req.Header.Set("Destination", "http://localhost/copy.txt")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("Expected status %d, got %d", http.StatusCreated, rec.Code)
	}
}

func TestMove(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("MOVE", "/test.txt", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	req.Header.Set("Destination", "http://localhost/moved.txt")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("Expected status %d, got %d", http.StatusCreated, rec.Code)
	}
}

func TestOptions(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("OPTIONS", "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rec.Code)
	}

	dav := rec.Header().Get("DAV")
	if dav == "" {
		t.Error("Expected DAV header to be set")
	}

	allow := rec.Header().Get("Allow")
	if !strings.Contains(allow, "PROPFIND") {
		t.Errorf("Expected Allow header to contain PROPFIND, got %s", allow)
	}
}

func TestLock(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("LOCK", "/test.txt", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rec.Code)
	}

	lockToken := rec.Header().Get("Lock-Token")
	if lockToken == "" {
		t.Error("Expected Lock-Token header to be set")
	}
}

func TestUnlock(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("UNLOCK", "/test.txt", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	req.Header.Set("Lock-Token", "<opaquelocktoken:abc123>")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("Expected status %d, got %d", http.StatusNoContent, rec.Code)
	}
}

func TestUnauthorized(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	// No auth header
	req := httptest.NewRequest("PROPFIND", "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("Expected status %d, got %d", http.StatusUnauthorized, rec.Code)
	}

	wwwAuth := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(wwwAuth, "Basic") {
		t.Errorf("Expected WWW-Authenticate header with Basic, got %s", wwwAuth)
	}
}

func TestInvalidAuth(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("PROPFIND", "/", nil)
	req.Header.Set("Authorization", "Basic d3Jvbmc6cGFzc3dvcmQ=") // wrong:password
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("Expected status %d, got %d", http.StatusUnauthorized, rec.Code)
	}
}

func TestWithPrefix(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient, WithPrefix("/webdav"))

	req := httptest.NewRequest("PROPFIND", "/webdav/", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	req.Header.Set("Depth", "0")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Errorf("Expected status %d, got %d", http.StatusMultiStatus, rec.Code)
	}
}

func TestHeadRequest(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("HEAD", "/test.txt", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rec.Code)
	}

	// HEAD should return headers but no body
	body, _ := io.ReadAll(rec.Body)
	if len(body) != 0 {
		t.Errorf("Expected empty body for HEAD request, got %d bytes", len(body))
	}
}

func TestProppatch(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("PROPPATCH", "/test.txt", strings.NewReader(`<?xml version="1.0" encoding="utf-8"?>
<propertyupdate xmlns="DAV:">
  <set>
    <prop>
      <displayname>New Name</displayname>
    </prop>
  </set>
</propertyupdate>`))
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Errorf("Expected status %d, got %d", http.StatusMultiStatus, rec.Code)
	}
}

func TestNotFound(t *testing.T) {
	fbServer := mockFileBrowser(t)
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("GET", "/nonexistent.txt", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("Expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

// Test streaming - verify data is passed through without buffering
func TestStreamingDownload(t *testing.T) {
	// Create a server that writes data in chunks
	fbServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/login" {
			w.Write([]byte("test-token"))
			return
		}
		if r.URL.Path == "/api/resources/large.txt" {
			json.NewEncoder(w).Encode(client.FileInfo{
				Path:     "/large.txt",
				Name:     "large.txt",
				Size:     1000,
				IsDir:    false,
				Type:     "text",
				Modified: time.Now(),
			})
			return
		}
		if r.URL.Path == "/api/raw/large.txt" {
			// Write in chunks to simulate streaming
			flusher, ok := w.(http.Flusher)
			for i := 0; i < 10; i++ {
				w.Write([]byte(strings.Repeat("x", 100)))
				if ok {
					flusher.Flush()
				}
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	req := httptest.NewRequest("GET", "/large.txt", nil)
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rec.Code)
	}

	// Verify we got all the data
	if len(rec.Body.Bytes()) != 1000 {
		t.Errorf("Expected 1000 bytes, got %d", len(rec.Body.Bytes()))
	}
}

// Test streaming upload
func TestStreamingUpload(t *testing.T) {
	var receivedData []byte

	fbServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/login" {
			w.Write([]byte("test-token"))
			return
		}
		if r.URL.Path == "/api/resources/upload.txt" {
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method == http.MethodPost {
				receivedData, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fbServer.Close()

	fbClient := client.New(fbServer.URL)
	handler := New(fbClient)

	// Create a large payload
	payload := strings.Repeat("y", 10000)
	req := httptest.NewRequest("PUT", "/upload.txt", strings.NewReader(payload))
	req.Header.Set("Authorization", "Basic YWRtaW46cGFzc3dvcmQ=")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("Expected status %d, got %d", http.StatusCreated, rec.Code)
	}

	if len(receivedData) != 10000 {
		t.Errorf("Expected 10000 bytes received, got %d", len(receivedData))
	}
}
