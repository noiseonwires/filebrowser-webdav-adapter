# FileBrowser WebDAV Adapter

A WebDAV server that acts as an adapter between WebDAV clients and the FileBrowser API.

## Overview

This adapter allows you to connect to FileBrowser using any WebDAV client (Windows Explorer, macOS Finder, Cyberduck, WinSCP, etc.). The adapter translates WebDAV protocol requests into FileBrowser API calls.

## Features

- **Full WebDAV Support**: Implements all essential WebDAV methods (PROPFIND, GET, PUT, DELETE, MKCOL, COPY, MOVE)
- **Streaming Transfers**: Files are streamed directly between the WebDAV client and FileBrowser without being saved to disk
- **Authentication**: Supports Basic authentication which is translated to FileBrowser JWT tokens
- **Token Caching**: JWT tokens are cached and automatically renewed to improve performance

## Installation

### From Source

```bash
cd webdav-adapter
cargo build --release
# binary at target/release/webdav-adapter
```

## Usage

```bash
# Basic usage
./webdav-adapter --filebrowser-url http://localhost:8080 --listen :8081

# With custom settings
./webdav-adapter \
  --filebrowser-url http://localhost:8080 \
  --listen :8081 \
  --prefix /webdav \
  --debug
```

### Command Line Options

| Option | Default | Description |
|--------|---------|-------------|
| `--filebrowser-url` | `http://localhost:8080` | FileBrowser server URL |
| `--listen` | `:8081` | Address to listen on |
| `--prefix` | `/` | URL prefix for WebDAV requests |
| `--debug` | `false` | Enable debug logging |
| `--token-cache-ttl` | `1800` | JWT token cache TTL in seconds |

### Environment Variables

All options can also be set via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `FILEBROWSER_URL` | `http://localhost:8080` | FileBrowser server URL |
| `LISTEN_ADDR` | `:8081` | Address to listen on |
| `WEBDAV_PREFIX` | `/` | URL prefix for WebDAV requests |
| `DEBUG` | `false` | Enable debug logging |
| `TOKEN_CACHE_TTL` | `1800` | JWT token cache TTL in seconds |

## Connecting Clients

### Windows Explorer

1. Open File Explorer
2. Right-click "This PC" → "Map network drive..."
3. Enter the WebDAV URL: `http://localhost:8081/`
4. Check "Connect using different credentials"
5. Enter your FileBrowser username and password

### macOS Finder

1. Open Finder
2. Press `Cmd+K` or go to Go → Connect to Server
3. Enter: `http://localhost:8081/`
4. Enter your FileBrowser credentials

### Linux (GNOME Files)

1. Open Files
2. Press `Ctrl+L` to open location bar
3. Enter: `dav://localhost:8081/`
4. Enter your FileBrowser credentials

### Cyberduck / WinSCP

1. Create a new WebDAV connection
2. Server: `localhost`
3. Port: `8081`
4. Path: `/`
5. Username/Password: Your FileBrowser credentials

## Architecture

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│  WebDAV Client  │────>│  WebDAV Adapter │────>│   FileBrowser   │
│ (Explorer, etc) │<────│    (This app)   │<────│     Server      │
└─────────────────┘     └─────────────────┘     └─────────────────┘
      WebDAV                HTTP/WebDAV            HTTP/REST API
```

## WebDAV Method Mapping

| WebDAV Method | FileBrowser API | Description |
|---------------|-----------------|-------------|
| PROPFIND | `GET /api/resources/{path}` | List directory / Get file info |
| GET | `GET /api/raw/{path}` | Download file |
| PUT | `POST /api/resources/{path}` | Upload/create file |
| DELETE | `DELETE /api/resources/{path}` | Delete file/directory |
| MKCOL | `POST /api/resources/{path}/` | Create directory |
| COPY | `PATCH /api/resources/{path}?action=copy` | Copy file/directory |
| MOVE | `PATCH /api/resources/{path}?action=rename` | Move/rename file/directory |
| OPTIONS | N/A | Return supported methods |
| LOCK/UNLOCK | N/A | No-op (not supported by FileBrowser) |

## Limitations

- **No Locking**: FileBrowser doesn't support file locking, so LOCK/UNLOCK return success but don't actually lock files
- **No Partial Content**: Range requests are not supported for uploads
- **Single User Scope**: Each WebDAV connection operates within the user's configured scope in FileBrowser

## License

Same as FileBrowser - Apache License 2.0
