// Package webdav implements a WebDAV server that proxies to FileBrowser.
package webdav

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/noiseonwires/filebrowser-webdav-adapter/internal/client"
)

// Handler implements a WebDAV server that proxies to FileBrowser.
type Handler struct {
	client *client.Client
	prefix string
	debug  bool
	logger *log.Logger
}

// Option configures the handler.
type Option func(*Handler)

// WithPrefix sets the URL prefix to strip from incoming requests.
func WithPrefix(prefix string) Option {
	return func(h *Handler) {
		h.prefix = strings.TrimSuffix(prefix, "/")
	}
}

// WithDebug enables debug logging.
func WithDebug(debug bool) Option {
	return func(h *Handler) {
		h.debug = debug
	}
}

// WithLogger sets a custom logger.
func WithLogger(logger *log.Logger) Option {
	return func(h *Handler) {
		h.logger = logger
	}
}

// New creates a new WebDAV handler.
func New(fbClient *client.Client, opts ...Option) *Handler {
	h := &Handler{
		client: fbClient,
		prefix: "",
		logger: log.New(os.Stderr, "[webdav] ", log.LstdFlags),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ServeHTTP handles incoming WebDAV requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.debug {
		h.logger.Printf("%s %s", r.Method, r.URL.Path)
	}

	// OPTIONS doesn't require authentication (used for capability discovery)
	if r.Method == "OPTIONS" {
		h.handleOptions(w, r)
		return
	}

	// Extract auth from Basic auth header
	auth, ok := h.extractAuth(r)
	if !ok {
		h.requireAuth(w)
		return
	}

	// Strip prefix from path
	filePath := h.stripPrefix(r.URL.Path)
	if filePath == "" {
		filePath = "/"
	}

	// Handle WebDAV methods
	switch r.Method {
	case "PROPFIND":
		h.handlePropfind(w, r, auth, filePath)
	case "GET", "HEAD":
		h.handleGet(w, r, auth, filePath)
	case "PUT":
		h.handlePut(w, r, auth, filePath)
	case "DELETE":
		h.handleDelete(w, r, auth, filePath)
	case "MKCOL":
		h.handleMkcol(w, r, auth, filePath)
	case "COPY":
		h.handleCopy(w, r, auth, filePath)
	case "MOVE":
		h.handleMove(w, r, auth, filePath)
	case "LOCK":
		h.handleLock(w, r, auth, filePath)
	case "UNLOCK":
		h.handleUnlock(w, r)
	case "PROPPATCH":
		h.handleProppatch(w, r, auth, filePath)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) extractAuth(r *http.Request) (client.Auth, bool) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return client.Auth{}, false
	}

	if !strings.HasPrefix(authHeader, "Basic ") {
		return client.Auth{}, false
	}

	decoded, err := base64.StdEncoding.DecodeString(authHeader[6:])
	if err != nil {
		return client.Auth{}, false
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return client.Auth{}, false
	}

	return client.Auth{
		Username: parts[0],
		Password: parts[1],
	}, true
}

func (h *Handler) requireAuth(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="FileBrowser WebDAV"`)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

func (h *Handler) stripPrefix(reqPath string) string {
	if h.prefix == "" {
		return reqPath
	}
	return strings.TrimPrefix(reqPath, h.prefix)
}

func (h *Handler) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, MKCOL, COPY, MOVE, LOCK, UNLOCK")
	w.Header().Set("DAV", "1, 2")
	w.Header().Set("MS-Author-Via", "DAV")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handlePropfind(w http.ResponseWriter, r *http.Request, auth client.Auth, filePath string) {
	// Parse depth header (0, 1, or infinity)
	depth := r.Header.Get("Depth")
	if depth == "" {
		depth = "infinity"
	}

	ctx := r.Context()
	info, err := h.client.GetFileInfo(ctx, auth, filePath)
	if err != nil {
		h.handleError(w, err)
		return
	}

	// Build response
	responses := []propfindResponse{h.fileInfoToResponse(r, filePath, info)}

	// If directory and depth > 0, include children
	if info.IsDir && depth != "0" {
		for _, item := range info.Items {
			childPath := path.Join(filePath, item.Name)
			responses = append(responses, h.fileInfoToResponse(r, childPath, &item))
		}
	}

	// Write multistatus response
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)

	ms := multistatus{
		XMLNS:     "DAV:",
		Responses: responses,
	}
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(ms); err != nil {
		h.logger.Printf("Error encoding PROPFIND response: %v", err)
	}
}

func (h *Handler) fileInfoToResponse(r *http.Request, filePath string, info *client.FileInfo) propfindResponse {
	// Build href - must be properly URL-encoded
	href := h.prefix + encodePath(filePath)
	if info.IsDir && !strings.HasSuffix(href, "/") {
		href += "/"
	}

	// For root, use "/" as display name
	displayName := info.Name
	if displayName == "" {
		displayName = "/"
	}

	okProps := propstat{
		Status: "HTTP/1.1 200 OK",
		Prop: prop{
			DisplayName:     displayName,
			GetLastModified: info.Modified.UTC().Format(http.TimeFormat),
			CreationDate:    info.Modified.UTC().Format(time.RFC3339),
			ResourceType:    &resourceType{},
		},
	}

	if info.IsDir {
		okProps.Prop.ResourceType = &resourceType{Collection: &collection{}}
		okProps.Prop.GetContentType = "httpd/unix-directory"
	} else {
		okProps.Prop.GetContentLength = strconv.FormatInt(info.Size, 10)
		okProps.Prop.GetContentType = getMimeType(info.Name, info.Type)
		okProps.Prop.GetETag = fmt.Sprintf(`"%x-%x"`, info.Modified.Unix(), info.Size)
	}

	return propfindResponse{
		Href:     href,
		Propstat: []propstat{okProps},
	}
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request, auth client.Auth, filePath string) {
	ctx := r.Context()

	// First get file info to check if it's a directory
	info, err := h.client.GetFileInfo(ctx, auth, filePath)
	if err != nil {
		h.handleError(w, err)
		return
	}

	if info.IsDir {
		// Return directory listing as HTML
		h.handleDirectoryListing(w, r, info)
		return
	}

	// Stream file content
	body, contentLength, err := h.client.DownloadFile(ctx, auth, filePath)
	if err != nil {
		h.handleError(w, err)
		return
	}
	defer body.Close()

	// Set headers
	w.Header().Set("Content-Type", getMimeType(info.Name, info.Type))
	if contentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	}
	w.Header().Set("Last-Modified", info.Modified.UTC().Format(http.TimeFormat))
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, info.Modified.Unix(), info.Size))

	if r.Method == "HEAD" {
		return
	}

	// Stream content
	io.Copy(w, body)
}

func (h *Handler) handleDirectoryListing(w http.ResponseWriter, r *http.Request, info *client.FileInfo) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<!DOCTYPE html>\n<html><head><title>Index of %s</title></head><body>\n", info.Path)
	fmt.Fprintf(w, "<h1>Index of %s</h1>\n<hr>\n<pre>\n", info.Path)

	// Parent directory link
	if info.Path != "/" {
		fmt.Fprintf(w, `<a href="../">../</a>`+"\n")
	}

	for _, item := range info.Items {
		name := item.Name
		if item.IsDir {
			name += "/"
		}
		fmt.Fprintf(w, `<a href="%s">%s</a>`+"\n", url.PathEscape(name), name)
	}

	fmt.Fprintf(w, "</pre>\n<hr>\n</body></html>\n")
}

func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request, auth client.Auth, filePath string) {
	ctx := r.Context()

	// Check if file exists to determine create vs update
	_, err := h.client.GetFileInfo(ctx, auth, filePath)
	fileExists := err == nil

	if fileExists {
		// Update existing file
		err = h.client.UpdateFile(ctx, auth, filePath, r.Body)
	} else {
		// Create new file - stream directly from request body
		err = h.client.UploadFile(ctx, auth, filePath, r.Body, true)
	}

	if err != nil {
		h.handleError(w, err)
		return
	}

	if fileExists {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request, auth client.Auth, filePath string) {
	ctx := r.Context()

	err := h.client.Delete(ctx, auth, filePath)
	if err != nil {
		h.handleError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleMkcol(w http.ResponseWriter, r *http.Request, auth client.Auth, filePath string) {
	ctx := r.Context()

	// MKCOL must have empty body
	if r.ContentLength > 0 {
		http.Error(w, "MKCOL with body not supported", http.StatusUnsupportedMediaType)
		return
	}

	err := h.client.CreateDirectory(ctx, auth, filePath)
	if err != nil {
		h.handleError(w, err)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) handleCopy(w http.ResponseWriter, r *http.Request, auth client.Auth, filePath string) {
	ctx := r.Context()

	dst := h.getDestination(r)
	if dst == "" {
		http.Error(w, "Destination header required", http.StatusBadRequest)
		return
	}

	overwrite := r.Header.Get("Overwrite") != "F"

	// Check if destination exists
	_, err := h.client.GetFileInfo(ctx, auth, dst)
	dstExists := err == nil

	err = h.client.Copy(ctx, auth, filePath, dst, overwrite)
	if err != nil {
		h.handleError(w, err)
		return
	}

	if dstExists && overwrite {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (h *Handler) handleMove(w http.ResponseWriter, r *http.Request, auth client.Auth, filePath string) {
	ctx := r.Context()

	dst := h.getDestination(r)
	if dst == "" {
		http.Error(w, "Destination header required", http.StatusBadRequest)
		return
	}

	overwrite := r.Header.Get("Overwrite") != "F"

	// Check if destination exists
	_, err := h.client.GetFileInfo(ctx, auth, dst)
	dstExists := err == nil

	err = h.client.Move(ctx, auth, filePath, dst, overwrite)
	if err != nil {
		h.handleError(w, err)
		return
	}

	if dstExists && overwrite {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (h *Handler) getDestination(r *http.Request) string {
	dst := r.Header.Get("Destination")
	if dst == "" {
		return ""
	}

	// Parse destination URL
	dstURL, err := url.Parse(dst)
	if err != nil {
		return ""
	}

	// Extract path and strip prefix
	dstPath := dstURL.Path
	dstPath = h.stripPrefix(dstPath)

	// Remove trailing slash for consistency
	dstPath = strings.TrimSuffix(dstPath, "/")

	return dstPath
}

// handleLock handles LOCK requests.
// FileBrowser doesn't support locking, so we return a fake lock token.
func (h *Handler) handleLock(w http.ResponseWriter, r *http.Request, auth client.Auth, filePath string) {
	ctx := r.Context()

	// Verify the resource exists (or can be created for new files)
	_, err := h.client.GetFileInfo(ctx, auth, filePath)
	if err != nil && !errors.Is(err, client.ErrNotFound) {
		h.handleError(w, err)
		return
	}

	// Generate a fake lock token
	lockToken := fmt.Sprintf("opaquelocktoken:%x", time.Now().UnixNano())

	// Build lock discovery response
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Lock-Token", "<"+lockToken+">")
	w.WriteHeader(http.StatusOK)

	// Build href
	href := h.prefix + encodePath(filePath)

	lockResp := lockResponse{
		XMLNS: "DAV:",
		LockDiscovery: lockDiscovery{
			ActiveLock: activeLock{
				LockType:  lockType{Write: &struct{}{}},
				LockScope: lockScope{Exclusive: &struct{}{}},
				Depth:     "infinity",
				Owner:     owner{Href: auth.Username},
				Timeout:   "Second-3600",
				LockToken: lockTokenXML{Href: lockToken},
				LockRoot:  lockRoot{Href: href},
			},
		},
	}

	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	enc.Encode(lockResp)
}

// handleUnlock handles UNLOCK requests.
// FileBrowser doesn't support locking, so we just return success.
func (h *Handler) handleUnlock(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// handleProppatch handles PROPPATCH requests.
// We accept but ignore property changes since FileBrowser doesn't support custom properties.
func (h *Handler) handleProppatch(w http.ResponseWriter, r *http.Request, auth client.Auth, filePath string) {
	ctx := r.Context()

	// Verify the resource exists
	_, err := h.client.GetFileInfo(ctx, auth, filePath)
	if err != nil {
		h.handleError(w, err)
		return
	}

	// Return success for all property changes
	href := h.prefix + encodePath(filePath)

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)

	// Parse the request to see what properties were requested
	// For simplicity, we just acknowledge with success
	ms := multistatus{
		XMLNS: "DAV:",
		Responses: []propfindResponse{
			{
				Href: href,
				Propstat: []propstat{
					{
						Status: "HTTP/1.1 200 OK",
						Prop:   prop{},
					},
				},
			},
		},
	}

	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	enc.Encode(ms)
}

func (h *Handler) handleError(w http.ResponseWriter, err error) {
	if h.debug {
		h.logger.Printf("Error: %v", err)
	}

	switch {
	case errors.Is(err, client.ErrUnauthorized):
		h.requireAuth(w)
	case errors.Is(err, client.ErrNotFound):
		http.Error(w, "Not Found", http.StatusNotFound)
	case errors.Is(err, client.ErrForbidden):
		http.Error(w, "Forbidden", http.StatusForbidden)
	case errors.Is(err, client.ErrConflict):
		http.Error(w, "Conflict", http.StatusConflict)
	case errors.Is(err, client.ErrBadRequest):
		http.Error(w, "Bad Request", http.StatusBadRequest)
	case errors.Is(err, context.Canceled):
		// Client disconnected, don't send response
		return
	default:
		h.logger.Printf("Internal error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func encodePath(p string) string {
	// Ensure path starts with /
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}

	// Split path and encode each segment
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/")
}

func getMimeType(name, fileType string) string {
	// Map FileBrowser types to MIME types
	switch fileType {
	case "text", "textImmutable":
		return "text/plain; charset=utf-8"
	case "image":
		ext := strings.ToLower(path.Ext(name))
		switch ext {
		case ".jpg", ".jpeg":
			return "image/jpeg"
		case ".png":
			return "image/png"
		case ".gif":
			return "image/gif"
		case ".webp":
			return "image/webp"
		case ".svg":
			return "image/svg+xml"
		default:
			return "image/jpeg"
		}
	case "video":
		ext := strings.ToLower(path.Ext(name))
		switch ext {
		case ".mp4":
			return "video/mp4"
		case ".webm":
			return "video/webm"
		case ".mkv":
			return "video/x-matroska"
		case ".avi":
			return "video/x-msvideo"
		default:
			return "video/mp4"
		}
	case "audio":
		ext := strings.ToLower(path.Ext(name))
		switch ext {
		case ".mp3":
			return "audio/mpeg"
		case ".wav":
			return "audio/wav"
		case ".ogg":
			return "audio/ogg"
		case ".flac":
			return "audio/flac"
		default:
			return "audio/mpeg"
		}
	case "pdf":
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}
