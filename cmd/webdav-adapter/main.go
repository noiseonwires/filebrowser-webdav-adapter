// FileBrowser WebDAV Adapter
//
// This program provides a WebDAV interface to FileBrowser, allowing users to
// connect using any WebDAV client (Windows Explorer, macOS Finder, etc.).
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/noiseonwires/filebrowser-webdav-adapter/internal/client"
	"github.com/noiseonwires/filebrowser-webdav-adapter/internal/webdav"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// Parse command line flags
	var (
		filebrowserURL = flag.String("filebrowser-url", getEnv("FILEBROWSER_URL", "http://localhost:8080"), "FileBrowser server URL")
		listenAddr     = flag.String("listen", getEnv("LISTEN_ADDR", ":8081"), "Address to listen on")
		prefix         = flag.String("prefix", getEnv("WEBDAV_PREFIX", "/"), "URL prefix for WebDAV requests")
		debug          = flag.Bool("debug", getEnvBool("DEBUG", false), "Enable debug logging")
		tokenCacheTTL  = flag.Duration("token-cache-ttl", getEnvDuration("TOKEN_CACHE_TTL", 30*time.Minute), "JWT token cache TTL")
		showVersion    = flag.Bool("version", false, "Show version information")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("webdav-adapter %s (commit: %s, built: %s)\n", version, commit, date)
		os.Exit(0)
	}

	// Normalize FileBrowser URL
	*filebrowserURL = strings.TrimSuffix(*filebrowserURL, "/")

	// Create logger
	logger := log.New(os.Stderr, "", log.LstdFlags)

	// Log configuration
	logger.Printf("FileBrowser WebDAV Adapter starting...")
	logger.Printf("  FileBrowser URL: %s", *filebrowserURL)
	logger.Printf("  Listen address:  %s", *listenAddr)
	logger.Printf("  URL prefix:      %s", *prefix)
	logger.Printf("  Debug mode:      %v", *debug)
	logger.Printf("  Token cache TTL: %s", *tokenCacheTTL)

	// Create FileBrowser API client
	fbClient := client.New(
		*filebrowserURL,
		client.WithDebug(*debug),
		client.WithTokenCacheTTL(*tokenCacheTTL),
	)

	// Create WebDAV handler
	handler := webdav.New(
		fbClient,
		webdav.WithPrefix(*prefix),
		webdav.WithDebug(*debug),
		webdav.WithLogger(log.New(os.Stderr, "[webdav] ", log.LstdFlags)),
	)

	// Create HTTP server
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"OK"}`))
	})

	// WebDAV handler at the configured prefix
	if *prefix == "/" {
		mux.Handle("/", handler)
	} else {
		mux.Handle(*prefix+"/", handler)
		mux.Handle(*prefix, handler)
		// Also handle root for clients that don't use the prefix
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				// Redirect to prefix
				http.Redirect(w, r, *prefix+"/", http.StatusMovedPermanently)
				return
			}
			http.NotFound(w, r)
		})
	}

	server := &http.Server{
		Addr:         *listenAddr,
		Handler:      mux,
		ReadTimeout:  0, // No timeout for streaming uploads
		WriteTimeout: 0, // No timeout for streaming downloads
		IdleTimeout:  120 * time.Second,
	}

	logger.Printf("WebDAV server listening on %s", *listenAddr)
	if err := server.ListenAndServe(); err != nil {
		logger.Fatalf("Server error: %v", err)
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		return strings.ToLower(value) == "true" || value == "1"
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
	}
	return defaultValue
}
