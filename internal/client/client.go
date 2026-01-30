// Package client provides a FileBrowser API client with streaming support.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// FileInfo represents file/directory information from FileBrowser.
type FileInfo struct {
	Path      string     `json:"path"`
	Name      string     `json:"name"`
	Size      int64      `json:"size"`
	Extension string     `json:"extension"`
	Modified  time.Time  `json:"modified"`
	Mode      uint32     `json:"mode"`
	IsDir     bool       `json:"isDir"`
	IsSymlink bool       `json:"isSymlink"`
	Type      string     `json:"type"`
	Content   string     `json:"content,omitempty"`
	Items     []FileInfo `json:"items,omitempty"`
	NumDirs   int        `json:"numDirs,omitempty"`
	NumFiles  int        `json:"numFiles,omitempty"`
}

// Client is a FileBrowser API client.
type Client struct {
	baseURL    string
	httpClient *http.Client
	debug      bool

	// Token cache
	tokenCache     map[string]*tokenEntry
	tokenCacheMu   sync.RWMutex
	tokenCacheTTL  time.Duration
}

type tokenEntry struct {
	token     string
	expiresAt time.Time
}

// Option configures the client.
type Option func(*Client)

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(client *Client) {
		client.httpClient = c
	}
}

// WithDebug enables debug logging.
func WithDebug(debug bool) Option {
	return func(client *Client) {
		client.debug = debug
	}
}

// WithTokenCacheTTL sets the token cache TTL.
func WithTokenCacheTTL(ttl time.Duration) Option {
	return func(client *Client) {
		client.tokenCacheTTL = ttl
	}
}

// New creates a new FileBrowser API client.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 0, // No timeout for streaming
		},
		tokenCache:    make(map[string]*tokenEntry),
		tokenCacheTTL: 30 * time.Minute,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Auth represents authentication credentials.
type Auth struct {
	Username string
	Password string
}

// cacheKey returns a cache key for the given auth.
func (a Auth) cacheKey() string {
	return a.Username + ":" + a.Password
}

// Login authenticates with FileBrowser and returns a JWT token.
// The token is cached for subsequent requests.
func (c *Client) Login(ctx context.Context, auth Auth) (string, error) {
	// Check cache first
	c.tokenCacheMu.RLock()
	entry, ok := c.tokenCache[auth.cacheKey()]
	c.tokenCacheMu.RUnlock()

	if ok && time.Now().Before(entry.expiresAt) {
		return entry.token, nil
	}

	// Login to get a new token
	loginURL := c.baseURL + "/api/login"
	payload := fmt.Sprintf(`{"username":%q,"password":%q}`, auth.Username, auth.Password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("creating login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return "", ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("login failed with status %d: %s", resp.StatusCode, string(body))
	}

	tokenBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading token: %w", err)
	}

	token := strings.TrimSpace(string(tokenBytes))

	// Cache the token
	c.tokenCacheMu.Lock()
	c.tokenCache[auth.cacheKey()] = &tokenEntry{
		token:     token,
		expiresAt: time.Now().Add(c.tokenCacheTTL),
	}
	c.tokenCacheMu.Unlock()

	return token, nil
}

// InvalidateToken removes a token from the cache.
func (c *Client) InvalidateToken(auth Auth) {
	c.tokenCacheMu.Lock()
	delete(c.tokenCache, auth.cacheKey())
	c.tokenCacheMu.Unlock()
}

// GetFileInfo retrieves file or directory information.
func (c *Client) GetFileInfo(ctx context.Context, auth Auth, path string) (*FileInfo, error) {
	token, err := c.Login(ctx, auth)
	if err != nil {
		return nil, err
	}

	reqURL := c.baseURL + "/api/resources" + encodePath(path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("X-Auth", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		c.InvalidateToken(auth)
		return nil, ErrUnauthorized
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, ErrForbidden
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var info FileInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &info, nil
}

// DownloadFile streams file content from FileBrowser.
// The caller is responsible for closing the returned ReadCloser.
func (c *Client) DownloadFile(ctx context.Context, auth Auth, path string) (io.ReadCloser, int64, error) {
	token, err := c.Login(ctx, auth)
	if err != nil {
		return nil, 0, err
	}

	reqURL := c.baseURL + "/api/raw" + encodePath(path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("X-Auth", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		c.InvalidateToken(auth)
		return nil, 0, ErrUnauthorized
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, 0, ErrNotFound
	}
	if resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		return nil, 0, ErrForbidden
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, 0, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, resp.ContentLength, nil
}

// UploadFile streams file content to FileBrowser.
func (c *Client) UploadFile(ctx context.Context, auth Auth, path string, content io.Reader, overwrite bool) error {
	token, err := c.Login(ctx, auth)
	if err != nil {
		return err
	}

	reqURL := c.baseURL + "/api/resources" + encodePath(path)
	if overwrite {
		reqURL += "?override=true"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, content)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("X-Auth", token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		c.InvalidateToken(auth)
		return ErrUnauthorized
	}
	if resp.StatusCode == http.StatusForbidden {
		return ErrForbidden
	}
	if resp.StatusCode == http.StatusConflict {
		return ErrConflict
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// UpdateFile updates an existing file's content.
func (c *Client) UpdateFile(ctx context.Context, auth Auth, path string, content io.Reader) error {
	token, err := c.Login(ctx, auth)
	if err != nil {
		return err
	}

	reqURL := c.baseURL + "/api/resources" + encodePath(path)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, content)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("X-Auth", token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		c.InvalidateToken(auth)
		return ErrUnauthorized
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode == http.StatusForbidden {
		return ErrForbidden
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// CreateDirectory creates a new directory.
func (c *Client) CreateDirectory(ctx context.Context, auth Auth, path string) error {
	token, err := c.Login(ctx, auth)
	if err != nil {
		return err
	}

	// Directory paths must end with /
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}

	reqURL := c.baseURL + "/api/resources" + encodePath(path)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("X-Auth", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		c.InvalidateToken(auth)
		return ErrUnauthorized
	}
	if resp.StatusCode == http.StatusForbidden {
		return ErrForbidden
	}
	if resp.StatusCode == http.StatusConflict {
		return ErrConflict
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// Delete removes a file or directory.
func (c *Client) Delete(ctx context.Context, auth Auth, path string) error {
	token, err := c.Login(ctx, auth)
	if err != nil {
		return err
	}

	reqURL := c.baseURL + "/api/resources" + encodePath(path)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("X-Auth", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		c.InvalidateToken(auth)
		return ErrUnauthorized
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode == http.StatusForbidden {
		return ErrForbidden
	}
	// Success can be 200 OK or 204 No Content
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// Copy copies a file or directory to a new location.
func (c *Client) Copy(ctx context.Context, auth Auth, srcPath, dstPath string, overwrite bool) error {
	return c.moveOrCopy(ctx, auth, srcPath, dstPath, "copy", overwrite)
}

// Move moves/renames a file or directory.
func (c *Client) Move(ctx context.Context, auth Auth, srcPath, dstPath string, overwrite bool) error {
	return c.moveOrCopy(ctx, auth, srcPath, dstPath, "rename", overwrite)
}

func (c *Client) moveOrCopy(ctx context.Context, auth Auth, srcPath, dstPath, action string, overwrite bool) error {
	token, err := c.Login(ctx, auth)
	if err != nil {
		return err
	}

	// Build URL with query parameters
	reqURL := c.baseURL + "/api/resources" + encodePath(srcPath)
	params := url.Values{}
	params.Set("action", action)
	params.Set("destination", dstPath)
	if overwrite {
		params.Set("override", "true")
	}
	reqURL += "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("X-Auth", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		c.InvalidateToken(auth)
		return ErrUnauthorized
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode == http.StatusForbidden {
		return ErrForbidden
	}
	if resp.StatusCode == http.StatusConflict {
		return ErrConflict
	}
	if resp.StatusCode == http.StatusBadRequest {
		return ErrBadRequest
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// encodePath properly encodes a path for URL usage.
func encodePath(path string) string {
	// Ensure path starts with /
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	
	// Split path and encode each segment
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/")
}
