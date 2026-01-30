//! End-to-end tests driving the adapter against a mock FileBrowser server.

use std::convert::Infallible;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;

use bytes::Bytes;
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::server::conn::http1;
use hyper::service::service_fn;
use hyper::{Method, Request, Response, StatusCode};
use hyper_util::rt::TokioIo;
use percent_encoding::percent_decode_str;
use reqwest::Client;
use tokio::net::TcpListener;
use webdav_adapter::server::{serve, App, Config};

const ADMIN_AUTH: &str = "Basic YWRtaW46cGFzc3dvcmQ="; // admin:password
const WRONG_AUTH: &str = "Basic d3Jvbmc6cGFzc3dvcmQ="; // wrong:password

const ROOT_JSON: &str = r#"{"path":"/","name":"","size":0,"modified":"2024-01-01T00:00:00Z","isDir":true,"type":"","items":[
  {"path":"/documents","name":"documents","size":0,"modified":"2024-01-01T00:00:00Z","isDir":true,"type":""},
  {"path":"/test.txt","name":"test.txt","size":13,"modified":"2024-01-01T00:00:00Z","isDir":false,"type":"text"}
]}"#;

const DOCUMENTS_JSON: &str = r#"{"path":"/documents","name":"documents","size":0,"modified":"2024-01-01T00:00:00Z","isDir":true,"type":"","items":[
  {"path":"/documents/file1.txt","name":"file1.txt","size":100,"modified":"2024-01-01T00:00:00Z","isDir":false,"type":"text"}
]}"#;

const TEST_TXT_JSON: &str =
    r#"{"path":"/test.txt","name":"test.txt","size":13,"modified":"2024-01-01T00:00:00Z","isDir":false,"type":"text"}"#;

/// Builds a minimal file-info JSON document for an arbitrary resource.
fn file_json(path: &str, name: &str, size: u64, is_dir: bool) -> String {
    format!(
        r#"{{"path":"{path}","name":"{name}","size":{size},"modified":"2024-01-01T00:00:00Z","isDir":{is_dir},"type":""}}"#
    )
}

fn text(status: StatusCode, body: &str) -> Response<Full<Bytes>> {
    Response::builder()
        .status(status)
        .body(Full::new(Bytes::from(body.to_string())))
        .unwrap()
}

fn json(body: &str) -> Response<Full<Bytes>> {
    Response::builder()
        .status(StatusCode::OK)
        .header("Content-Type", "application/json")
        .body(Full::new(Bytes::from(body.to_string())))
        .unwrap()
}

fn status(status: StatusCode) -> Response<Full<Bytes>> {
    Response::builder()
        .status(status)
        .body(Full::new(Bytes::new()))
        .unwrap()
}

async fn handle_mock(
    req: Request<Incoming>,
    login_count: Arc<AtomicUsize>,
) -> Result<Response<Full<Bytes>>, Infallible> {
    let method = req.method().clone();
    let path = req.uri().path().to_string();

    if path == "/api/login" {
        login_count.fetch_add(1, Ordering::SeqCst);
        let body = req.collect().await.unwrap().to_bytes();
        let creds: serde_json::Value =
            serde_json::from_slice(&body).unwrap_or(serde_json::Value::Null);
        if creds["username"] == "admin" && creds["password"] == "password" {
            return Ok(text(StatusCode::OK, "test-token"));
        }
        return Ok(status(StatusCode::FORBIDDEN));
    }

    if req.headers().get("X-Auth").and_then(|h| h.to_str().ok()) != Some("test-token") {
        return Ok(status(StatusCode::UNAUTHORIZED));
    }

    if let Some(rest) = path.strip_prefix("/api/raw") {
        // Decode once so we can detect any double-encoding from the adapter.
        let resource = percent_decode_str(rest).decode_utf8_lossy().into_owned();
        let body = match resource.as_str() {
            "/test.txt" => "Hello, World!".to_string(),
            "/documents/file1.txt" => "x".repeat(100),
            "/My File.txt" => "spaced content".to_string(),
            "/документ.txt" => "unicode content".to_string(),
            _ => return Ok(status(StatusCode::NOT_FOUND)),
        };

        // Conditional request short-circuits to 304.
        if req.headers().contains_key("if-none-match") {
            return Ok(status(StatusCode::NOT_MODIFIED));
        }

        // Honour a simple `bytes=start-end` range with a 206 response.
        if let Some(range) = req.headers().get("range").and_then(|v| v.to_str().ok()) {
            if let Some((start, end)) = parse_range(range, body.len()) {
                let slice = body[start..=end].to_string();
                let resp = Response::builder()
                    .status(StatusCode::PARTIAL_CONTENT)
                    .header(
                        "Content-Range",
                        format!("bytes {start}-{end}/{}", body.len()),
                    )
                    .body(Full::new(Bytes::from(slice)))
                    .unwrap();
                return Ok(resp);
            }
        }

        return Ok(text(StatusCode::OK, &body));
    }

    if let Some(rest) = path.strip_prefix("/api/resources") {
        let decoded = percent_decode_str(rest).decode_utf8_lossy().into_owned();
        let mut resource = decoded.trim_end_matches('/');
        if resource.is_empty() {
            resource = "/";
        }
        return Ok(match method {
            Method::GET => match resource {
                "/" => json(ROOT_JSON),
                "/documents" => json(DOCUMENTS_JSON),
                "/test.txt" => json(TEST_TXT_JSON),
                "/My File.txt" => json(&file_json("/My File.txt", "My File.txt", 14, false)),
                "/документ.txt" => json(&file_json("/документ.txt", "документ.txt", 15, false)),
                "/existing-dir" => json(&file_json("/existing-dir", "existing-dir", 0, true)),
                _ => status(StatusCode::NOT_FOUND),
            },
            // Creation (MKCOL / upload) replies 201 to exercise 2xx acceptance.
            Method::POST => status(StatusCode::CREATED),
            // Deletion replies 204 No Content.
            Method::DELETE => status(StatusCode::NO_CONTENT),
            // Move/copy replies 200.
            _ => status(StatusCode::OK),
        });
    }

    Ok(status(StatusCode::NOT_FOUND))
}

/// Parses a single closed `bytes=start-end` range, clamped to `len`.
fn parse_range(value: &str, len: usize) -> Option<(usize, usize)> {
    let spec = value.strip_prefix("bytes=")?;
    let (start, end) = spec.split_once('-')?;
    let start: usize = start.parse().ok()?;
    let end: usize = if end.is_empty() {
        len.saturating_sub(1)
    } else {
        end.parse().ok()?
    };
    if start > end || start >= len {
        return None;
    }
    Some((start, end.min(len - 1)))
}

async fn spawn_mock() -> (SocketAddr, Arc<AtomicUsize>) {
    let login_count = Arc::new(AtomicUsize::new(0));
    let counter = login_count.clone();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();

    tokio::spawn(async move {
        loop {
            let (stream, _) = listener.accept().await.unwrap();
            let counter = counter.clone();
            tokio::spawn(async move {
                let service = service_fn(move |req| handle_mock(req, counter.clone()));
                let _ = http1::Builder::new()
                    .serve_connection(TokioIo::new(stream), service)
                    .await;
            });
        }
    });

    (addr, login_count)
}

async fn spawn_adapter(filebrowser_url: String, prefix: &str) -> SocketAddr {
    let config = Config {
        filebrowser_url,
        listen: ":0".to_string(),
        prefix: prefix.to_string(),
        debug: false,
        token_cache_ttl_secs: 3600,
    };
    let app = App::new(&config);
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(serve(listener, app));
    addr
}

async fn setup(prefix: &str) -> (SocketAddr, Arc<AtomicUsize>) {
    let (mock_addr, login_count) = spawn_mock().await;
    let adapter = spawn_adapter(format!("http://{mock_addr}"), prefix).await;
    (adapter, login_count)
}

fn method(name: &str) -> Method {
    Method::from_bytes(name.as_bytes()).unwrap()
}

fn url(addr: SocketAddr, path: &str) -> String {
    format!("http://{addr}{path}")
}

#[tokio::test]
async fn options_advertises_dav() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(Method::OPTIONS, url(addr, "/"))
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    assert!(resp.headers().contains_key("DAV"));
    let allow = resp.headers()["Allow"].to_str().unwrap();
    assert!(allow.contains("PROPFIND"));
}

#[tokio::test]
async fn unauthorized_without_auth() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(method("PROPFIND"), url(addr, "/"))
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    assert!(resp.headers()["WWW-Authenticate"]
        .to_str()
        .unwrap()
        .contains("Basic"));
}

#[tokio::test]
async fn invalid_auth_rejected() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(method("PROPFIND"), url(addr, "/"))
        .header("Authorization", WRONG_AUTH)
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn propfind_root_depth_one() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(method("PROPFIND"), url(addr, "/"))
        .header("Authorization", ADMIN_AUTH)
        .header("Depth", "1")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::MULTI_STATUS);
    let body = resp.text().await.unwrap();
    assert_eq!(body.matches("<D:response>").count(), 3);
}

#[tokio::test]
async fn propfind_depth_zero() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(method("PROPFIND"), url(addr, "/"))
        .header("Authorization", ADMIN_AUTH)
        .header("Depth", "0")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::MULTI_STATUS);
    let body = resp.text().await.unwrap();
    assert_eq!(body.matches("<D:response>").count(), 1);
}

#[tokio::test]
async fn get_file_returns_content() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .get(url(addr, "/test.txt"))
        .header("Authorization", ADMIN_AUTH)
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(resp.text().await.unwrap(), "Hello, World!");
}

#[tokio::test]
async fn get_directory_returns_html() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .get(url(addr, "/documents"))
        .header("Authorization", ADMIN_AUTH)
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    assert!(resp.headers()["Content-Type"]
        .to_str()
        .unwrap()
        .contains("text/html"));
}

#[tokio::test]
async fn put_new_file_created() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .put(url(addr, "/newfile.txt"))
        .header("Authorization", ADMIN_AUTH)
        .body("New file content")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::CREATED);
}

#[tokio::test]
async fn mkcol_created() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(method("MKCOL"), url(addr, "/newfolder"))
        .header("Authorization", ADMIN_AUTH)
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::CREATED);
}

#[tokio::test]
async fn delete_no_content() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(Method::DELETE, url(addr, "/test.txt"))
        .header("Authorization", ADMIN_AUTH)
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::NO_CONTENT);
}

#[tokio::test]
async fn copy_created() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(method("COPY"), url(addr, "/test.txt"))
        .header("Authorization", ADMIN_AUTH)
        .header("Destination", "http://localhost/copy.txt")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::CREATED);
}

#[tokio::test]
async fn move_created() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(method("MOVE"), url(addr, "/test.txt"))
        .header("Authorization", ADMIN_AUTH)
        .header("Destination", "http://localhost/moved.txt")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::CREATED);
}

#[tokio::test]
async fn lock_returns_token() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(method("LOCK"), url(addr, "/test.txt"))
        .header("Authorization", ADMIN_AUTH)
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    assert!(resp.headers().contains_key("Lock-Token"));
}

#[tokio::test]
async fn unlock_no_content() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(method("UNLOCK"), url(addr, "/test.txt"))
        .header("Authorization", ADMIN_AUTH)
        .header("Lock-Token", "<opaquelocktoken:abc123>")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::NO_CONTENT);
}

#[tokio::test]
async fn head_has_no_body() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(Method::HEAD, url(addr, "/test.txt"))
        .header("Authorization", ADMIN_AUTH)
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    assert!(resp.bytes().await.unwrap().is_empty());
}

#[tokio::test]
async fn proppatch_multistatus() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(method("PROPPATCH"), url(addr, "/test.txt"))
        .header("Authorization", ADMIN_AUTH)
        .body("<propertyupdate/>")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::MULTI_STATUS);
}

#[tokio::test]
async fn get_missing_file_not_found() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .get(url(addr, "/nonexistent.txt"))
        .header("Authorization", ADMIN_AUTH)
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn token_is_cached() {
    let (addr, login_count) = setup("/").await;
    let client = Client::new();
    for _ in 0..5 {
        let resp = client
            .get(url(addr, "/test.txt"))
            .header("Authorization", ADMIN_AUTH)
            .send()
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
    }

    assert_eq!(login_count.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn prefix_mount_serves() {
    let (addr, _) = setup("/webdav").await;
    let resp = Client::new()
        .request(method("PROPFIND"), url(addr, "/webdav/"))
        .header("Authorization", ADMIN_AUTH)
        .header("Depth", "0")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::MULTI_STATUS);
}

#[tokio::test]
async fn health_endpoint() {
    let (addr, _) = setup("/webdav").await;
    let resp = Client::new().get(url(addr, "/health")).send().await.unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(resp.text().await.unwrap(), "{\"status\":\"OK\"}");
}

#[tokio::test]
async fn encoded_filename_round_trips() {
    let (addr, _) = setup("/").await;
    let client = Client::new();

    // Space in the name: the adapter must single-encode so the mock decodes
    // back to "My File.txt" (double-encoding would yield "My%20File.txt").
    let resp = client
        .get(url(addr, "/My File.txt"))
        .header("Authorization", ADMIN_AUTH)
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(resp.text().await.unwrap(), "spaced content");

    // Unicode name.
    let resp = client
        .get(url(addr, "/документ.txt"))
        .header("Authorization", ADMIN_AUTH)
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(resp.text().await.unwrap(), "unicode content");
}

#[tokio::test]
async fn range_request_is_forwarded() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .get(url(addr, "/test.txt"))
        .header("Authorization", ADMIN_AUTH)
        .header("Range", "bytes=0-4")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::PARTIAL_CONTENT);
    assert_eq!(
        resp.headers()["Content-Range"].to_str().unwrap(),
        "bytes 0-4/13"
    );
    assert_eq!(resp.headers()["Accept-Ranges"].to_str().unwrap(), "bytes");
    assert_eq!(resp.text().await.unwrap(), "Hello");
}

#[tokio::test]
async fn conditional_request_returns_not_modified() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .get(url(addr, "/test.txt"))
        .header("Authorization", ADMIN_AUTH)
        .header("If-None-Match", "\"anything\"")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::NOT_MODIFIED);
    assert!(resp.bytes().await.unwrap().is_empty());
}

#[tokio::test]
async fn mkcol_on_existing_is_not_allowed() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(method("MKCOL"), url(addr, "/existing-dir"))
        .header("Authorization", ADMIN_AUTH)
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::METHOD_NOT_ALLOWED);
    assert!(resp.headers().contains_key("Allow"));
}

#[tokio::test]
async fn mkcol_missing_parent_conflicts() {
    let (addr, _) = setup("/").await;
    let resp = Client::new()
        .request(method("MKCOL"), url(addr, "/nope/child"))
        .header("Authorization", ADMIN_AUTH)
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::CONFLICT);
}

/// Mock that rejects the first authenticated resource request with 401 to
/// exercise the adapter's token refresh-and-retry path.
async fn handle_expiring_mock(
    req: Request<Incoming>,
    login_count: Arc<AtomicUsize>,
    rejected: Arc<AtomicUsize>,
) -> Result<Response<Full<Bytes>>, Infallible> {
    let path = req.uri().path().to_string();

    if path == "/api/login" {
        login_count.fetch_add(1, Ordering::SeqCst);
        return Ok(text(StatusCode::OK, "test-token"));
    }

    if req.headers().get("X-Auth").and_then(|h| h.to_str().ok()) != Some("test-token") {
        return Ok(status(StatusCode::UNAUTHORIZED));
    }

    // Reject exactly the first authenticated request as if the token expired.
    if rejected.fetch_add(1, Ordering::SeqCst) == 0 {
        return Ok(status(StatusCode::UNAUTHORIZED));
    }

    if req.uri().path() == "/api/resources/test.txt" {
        return Ok(json(TEST_TXT_JSON));
    }
    if req.uri().path() == "/api/raw/test.txt" {
        return Ok(text(StatusCode::OK, "Hello, World!"));
    }
    Ok(status(StatusCode::NOT_FOUND))
}

#[tokio::test]
async fn expired_token_triggers_retry() {
    let login_count = Arc::new(AtomicUsize::new(0));
    let rejected = Arc::new(AtomicUsize::new(0));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let mock_addr = listener.local_addr().unwrap();
    {
        let login_count = login_count.clone();
        let rejected = rejected.clone();
        tokio::spawn(async move {
            loop {
                let (stream, _) = listener.accept().await.unwrap();
                let login_count = login_count.clone();
                let rejected = rejected.clone();
                tokio::spawn(async move {
                    let service = service_fn(move |req| {
                        handle_expiring_mock(req, login_count.clone(), rejected.clone())
                    });
                    let _ = http1::Builder::new()
                        .serve_connection(TokioIo::new(stream), service)
                        .await;
                });
            }
        });
    }

    let adapter = spawn_adapter(format!("http://{mock_addr}"), "/").await;
    let resp = Client::new()
        .get(url(adapter, "/test.txt"))
        .header("Authorization", ADMIN_AUTH)
        .send()
        .await
        .unwrap();

    // The first upstream call gets a 401; the adapter re-logs in and retries.
    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(resp.text().await.unwrap(), "Hello, World!");
    assert_eq!(login_count.load(Ordering::SeqCst), 2);
}

