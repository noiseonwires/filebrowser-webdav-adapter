package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLogin(t *testing.T) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/login" {
			t.Errorf("Expected path /api/login, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST method, got %s", r.Method)
		}

		var payload struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		json.NewDecoder(r.Body).Decode(&payload)

		if payload.Username == "admin" && payload.Password == "password" {
			w.Write([]byte("test-jwt-token"))
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()

	client := New(server.URL)

	// Test successful login
	token, err := client.Login(context.Background(), Auth{Username: "admin", Password: "password"})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if token != "test-jwt-token" {
		t.Errorf("Expected token 'test-jwt-token', got '%s'", token)
	}

	// Test failed login
	_, err = client.Login(context.Background(), Auth{Username: "admin", Password: "wrong"})
	if err != ErrUnauthorized {
		t.Errorf("Expected ErrUnauthorized, got %v", err)
	}
}

func TestGetFileInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/login") {
			w.Write([]byte("test-token"))
			return
		}

		if r.Header.Get("X-Auth") != "test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if r.URL.Path == "/api/resources/test.txt" {
			info := FileInfo{
				Path:     "/test.txt",
				Name:     "test.txt",
				Size:     100,
				Modified: time.Now(),
				IsDir:    false,
				Type:     "text",
			}
			json.NewEncoder(w).Encode(info)
			return
		}

		if r.URL.Path == "/api/resources/folder" {
			info := FileInfo{
				Path:     "/folder",
				Name:     "folder",
				Size:     4096,
				Modified: time.Now(),
				IsDir:    true,
				Items: []FileInfo{
					{Path: "/folder/file1.txt", Name: "file1.txt", Size: 50, IsDir: false},
					{Path: "/folder/file2.txt", Name: "file2.txt", Size: 75, IsDir: false},
				},
			}
			json.NewEncoder(w).Encode(info)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := New(server.URL)
	auth := Auth{Username: "admin", Password: "password"}

	// Test getting file info
	info, err := client.GetFileInfo(context.Background(), auth, "/test.txt")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if info.Name != "test.txt" {
		t.Errorf("Expected name 'test.txt', got '%s'", info.Name)
	}
	if info.IsDir {
		t.Error("Expected IsDir to be false")
	}

	// Test getting directory info
	info, err = client.GetFileInfo(context.Background(), auth, "/folder")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if !info.IsDir {
		t.Error("Expected IsDir to be true")
	}
	if len(info.Items) != 2 {
		t.Errorf("Expected 2 items, got %d", len(info.Items))
	}

	// Test not found
	_, err = client.GetFileInfo(context.Background(), auth, "/nonexistent")
	if err != ErrNotFound {
		t.Errorf("Expected ErrNotFound, got %v", err)
	}
}

func TestDownloadFile(t *testing.T) {
	fileContent := "Hello, World!"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/login") {
			w.Write([]byte("test-token"))
			return
		}

		if r.Header.Get("X-Auth") != "test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if r.URL.Path == "/api/raw/test.txt" {
			w.Header().Set("Content-Length", "13")
			w.Write([]byte(fileContent))
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := New(server.URL)
	auth := Auth{Username: "admin", Password: "password"}

	body, length, err := client.DownloadFile(context.Background(), auth, "/test.txt")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	defer body.Close()

	if length != 13 {
		t.Errorf("Expected content length 13, got %d", length)
	}

	content, _ := io.ReadAll(body)
	if string(content) != fileContent {
		t.Errorf("Expected content '%s', got '%s'", fileContent, string(content))
	}
}

func TestUploadFile(t *testing.T) {
	var receivedContent string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/login") {
			w.Write([]byte("test-token"))
			return
		}

		if r.Header.Get("X-Auth") != "test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if r.Method == http.MethodPost && r.URL.Path == "/api/resources/newfile.txt" {
			content, _ := io.ReadAll(r.Body)
			receivedContent = string(content)
			w.WriteHeader(http.StatusOK)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := New(server.URL)
	auth := Auth{Username: "admin", Password: "password"}

	content := "New file content"
	err := client.UploadFile(context.Background(), auth, "/newfile.txt", strings.NewReader(content), false)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if receivedContent != content {
		t.Errorf("Expected content '%s', got '%s'", content, receivedContent)
	}
}

func TestCreateDirectory(t *testing.T) {
	var createdPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/login") {
			w.Write([]byte("test-token"))
			return
		}

		if r.Header.Get("X-Auth") != "test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/") {
			createdPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			return
		}

		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	client := New(server.URL)
	auth := Auth{Username: "admin", Password: "password"}

	err := client.CreateDirectory(context.Background(), auth, "/newfolder")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if createdPath != "/api/resources/newfolder/" {
		t.Errorf("Expected path '/api/resources/newfolder/', got '%s'", createdPath)
	}
}

func TestDelete(t *testing.T) {
	var deletedPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/login") {
			w.Write([]byte("test-token"))
			return
		}

		if r.Header.Get("X-Auth") != "test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if r.Method == http.MethodDelete {
			deletedPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			return
		}

		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	client := New(server.URL)
	auth := Auth{Username: "admin", Password: "password"}

	err := client.Delete(context.Background(), auth, "/file.txt")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if deletedPath != "/api/resources/file.txt" {
		t.Errorf("Expected path '/api/resources/file.txt', got '%s'", deletedPath)
	}
}

func TestMoveAndCopy(t *testing.T) {
	var lastRequest struct {
		method string
		path   string
		action string
		dest   string
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/login") {
			w.Write([]byte("test-token"))
			return
		}

		if r.Header.Get("X-Auth") != "test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if r.Method == http.MethodPatch {
			lastRequest.method = r.Method
			lastRequest.path = r.URL.Path
			lastRequest.action = r.URL.Query().Get("action")
			lastRequest.dest = r.URL.Query().Get("destination")
			w.WriteHeader(http.StatusOK)
			return
		}

		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	client := New(server.URL)
	auth := Auth{Username: "admin", Password: "password"}

	// Test Copy
	err := client.Copy(context.Background(), auth, "/source.txt", "/dest.txt", false)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if lastRequest.action != "copy" {
		t.Errorf("Expected action 'copy', got '%s'", lastRequest.action)
	}
	if lastRequest.dest != "/dest.txt" {
		t.Errorf("Expected destination '/dest.txt', got '%s'", lastRequest.dest)
	}

	// Test Move
	err = client.Move(context.Background(), auth, "/source.txt", "/dest.txt", false)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if lastRequest.action != "rename" {
		t.Errorf("Expected action 'rename', got '%s'", lastRequest.action)
	}
}

func TestTokenCaching(t *testing.T) {
	loginCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/login" {
			loginCount++
			w.Write([]byte("test-token"))
			return
		}
		if r.URL.Path == "/api/resources/test.txt" {
			json.NewEncoder(w).Encode(FileInfo{Name: "test.txt"})
			return
		}
	}))
	defer server.Close()

	client := New(server.URL, WithTokenCacheTTL(1*time.Hour))
	auth := Auth{Username: "admin", Password: "password"}

	// Multiple requests should only login once
	for i := 0; i < 5; i++ {
		_, err := client.GetFileInfo(context.Background(), auth, "/test.txt")
		if err != nil {
			t.Fatalf("Request %d failed: %v", i, err)
		}
	}

	if loginCount != 1 {
		t.Errorf("Expected 1 login, got %d", loginCount)
	}
}
