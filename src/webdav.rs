//! WebDAV server that proxies requests to FileBrowser.

use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use futures_util::StreamExt;
use http_body_util::BodyStream;
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use percent_encoding::percent_decode_str;
use time::format_description::well_known::Rfc3339;
use time::{OffsetDateTime, UtcOffset};

use crate::client::{self, Auth, Client, FileInfo};
use crate::util::{self, empty, encode_path, escape, full, log, Body};

const XML_HEADER: &str = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n";
const LOG_PREFIX: &str = "[webdav] ";

/// WebDAV request handler bound to a FileBrowser client.
pub struct Handler {
    client: Arc<Client>,
    /// Prefix stripped from incoming paths; empty means mounted at root.
    prefix: String,
    debug: bool,
}

impl Handler {
    pub fn new(client: Arc<Client>, prefix: String, debug: bool) -> Self {
        Handler {
            client,
            prefix,
            debug,
        }
    }

    /// Dispatches an incoming request to the matching WebDAV method.
    pub async fn serve(&self, req: Request<Incoming>) -> Response<Body> {
        if self.debug {
            log(LOG_PREFIX, &format!("{} {}", req.method(), req.uri().path()));
        }

        let method = req.method().clone();
        if method == Method::OPTIONS {
            return self.options();
        }

        let auth = match self.extract_auth(&req) {
            Some(auth) => auth,
            None => return self.require_auth(),
        };

        let mut file_path = self.strip_prefix(req.uri().path());
        // Incoming paths are percent-encoded on the wire; decode once so the
        // client re-encodes a single time (avoids double-escaping spaces,
        // Unicode, `%`, `#`, etc.).
        file_path = percent_decode_str(&file_path)
            .decode_utf8_lossy()
            .into_owned();
        if file_path.is_empty() {
            file_path = "/".to_string();
        }

        match method.as_str() {
            "PROPFIND" => self.propfind(&req, &auth, &file_path).await,
            "GET" => self.get(&req, false, &auth, &file_path).await,
            "HEAD" => self.get(&req, true, &auth, &file_path).await,
            "PUT" => self.put(req, &auth, &file_path).await,
            "DELETE" => self.delete(&auth, &file_path).await,
            "MKCOL" => self.mkcol(&req, &auth, &file_path).await,
            "COPY" => self.copy_or_move(&req, &auth, &file_path, false).await,
            "MOVE" => self.copy_or_move(&req, &auth, &file_path, true).await,
            "LOCK" => self.lock(&auth, &file_path).await,
            "UNLOCK" => status(StatusCode::NO_CONTENT),
            "PROPPATCH" => self.proppatch(&auth, &file_path).await,
            _ => text(StatusCode::METHOD_NOT_ALLOWED, "Method not allowed"),
        }
    }

    fn extract_auth(&self, req: &Request<Incoming>) -> Option<Auth> {
        let header = req.headers().get("Authorization")?.to_str().ok()?;
        let encoded = header.strip_prefix("Basic ")?;
        let decoded = STANDARD.decode(encoded).ok()?;
        let decoded = String::from_utf8(decoded).ok()?;
        let (username, password) = decoded.split_once(':')?;
        if self.debug {
            log(
                LOG_PREFIX,
                &format!(
                    "auth received: username={username:?} password_len={}",
                    password.len()
                ),
            );
        }
        Some(Auth {
            username: username.to_string(),
            password: password.to_string(),
        })
    }

    fn require_auth(&self) -> Response<Body> {
        Response::builder()
            .status(StatusCode::UNAUTHORIZED)
            .header("WWW-Authenticate", "Basic realm=\"FileBrowser WebDAV\"")
            .header("Content-Type", "text/plain; charset=utf-8")
            .body(full("Unauthorized\n"))
            .unwrap()
    }

    fn strip_prefix(&self, path: &str) -> String {
        if self.prefix.is_empty() {
            return path.to_string();
        }
        path.strip_prefix(&self.prefix)
            .map(str::to_string)
            .unwrap_or_else(|| path.to_string())
    }

    fn options(&self) -> Response<Body> {
        Response::builder()
            .status(StatusCode::OK)
            .header(
                "Allow",
                "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, MKCOL, COPY, MOVE, LOCK, UNLOCK",
            )
            .header("DAV", "1, 2")
            .header("MS-Author-Via", "DAV")
            .body(empty())
            .unwrap()
    }

    async fn propfind(
        &self,
        req: &Request<Incoming>,
        auth: &Auth,
        path: &str,
    ) -> Response<Body> {
        let depth = req
            .headers()
            .get("Depth")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("infinity");

        let info = match self.client.get_file_info(auth, path).await {
            Ok(info) => info,
            Err(err) => return self.error(err),
        };

        let mut xml = String::with_capacity(512);
        xml.push_str(XML_HEADER);
        xml.push_str("<D:multistatus xmlns:D=\"DAV:\">\n");
        self.write_response(&mut xml, path, &info);

        if info.is_dir && depth != "0" {
            for item in &info.items {
                let child = join_path(path, &item.name);
                self.write_response(&mut xml, &child, item);
            }
        }

        xml.push_str("</D:multistatus>\n");
        xml_response(StatusCode::MULTI_STATUS, xml)
    }

    fn write_response(&self, out: &mut String, path: &str, info: &FileInfo) {
        let mut href = format!("{}{}", self.prefix, encode_path(path));
        if info.is_dir && !href.ends_with('/') {
            href.push('/');
        }
        let display = if info.name.is_empty() { "/" } else { &info.name };

        out.push_str("  <D:response>\n");
        out.push_str(&format!("    <D:href>{}</D:href>\n", escape(&href)));
        out.push_str("    <D:propstat>\n      <D:prop>\n");
        out.push_str(&format!(
            "        <D:displayname>{}</D:displayname>\n",
            escape(display)
        ));

        if info.is_dir {
            out.push_str("        <D:getcontenttype>httpd/unix-directory</D:getcontenttype>\n");
            out.push_str(&format!(
                "        <D:getlastmodified>{}</D:getlastmodified>\n",
                http_date(info.modified)
            ));
            out.push_str(&format!(
                "        <D:creationdate>{}</D:creationdate>\n",
                rfc3339(info.modified)
            ));
            out.push_str(
                "        <D:resourcetype><D:collection></D:collection></D:resourcetype>\n",
            );
        } else {
            out.push_str(&format!(
                "        <D:getcontentlength>{}</D:getcontentlength>\n",
                info.size
            ));
            out.push_str(&format!(
                "        <D:getcontenttype>{}</D:getcontenttype>\n",
                mime_type(&info.name, &info.file_type)
            ));
            out.push_str(&format!(
                "        <D:getetag>{}</D:getetag>\n",
                escape(&etag(info))
            ));
            out.push_str(&format!(
                "        <D:getlastmodified>{}</D:getlastmodified>\n",
                http_date(info.modified)
            ));
            out.push_str(&format!(
                "        <D:creationdate>{}</D:creationdate>\n",
                rfc3339(info.modified)
            ));
            out.push_str("        <D:resourcetype></D:resourcetype>\n");
        }

        out.push_str("      </D:prop>\n      <D:status>HTTP/1.1 200 OK</D:status>\n");
        out.push_str("    </D:propstat>\n  </D:response>\n");
    }

    async fn get(
        &self,
        req: &Request<Incoming>,
        head: bool,
        auth: &Auth,
        path: &str,
    ) -> Response<Body> {
        let info = match self.client.get_file_info(auth, path).await {
            Ok(info) => info,
            Err(err) => return self.error(err),
        };

        if info.is_dir {
            return self.directory_listing(&info);
        }

        // Forward range and conditional headers so resume, media seeking and
        // optimistic concurrency work end to end.
        let mut forward: Vec<(&str, String)> = Vec::new();
        for name in [
            "range",
            "if-range",
            "if-match",
            "if-none-match",
            "if-modified-since",
            "if-unmodified-since",
        ] {
            if let Some(value) = req.headers().get(name).and_then(|v| v.to_str().ok()) {
                forward.push((name, value.to_string()));
            }
        }

        let resp = match self.client.download_file(auth, path, &forward).await {
            Ok(resp) => resp,
            Err(err) => return self.error(err),
        };

        let upstream = resp.status();
        let status = StatusCode::from_u16(upstream.as_u16()).unwrap_or(StatusCode::OK);
        let content_length = resp.content_length();
        let content_range = header_value(&resp, "content-range");

        let mut builder = Response::builder()
            .status(status)
            .header("Content-Type", mime_type(&info.name, &info.file_type))
            .header("Last-Modified", http_date(info.modified))
            .header("ETag", etag(&info))
            .header("Accept-Ranges", "bytes");

        if let Some(range) = content_range {
            builder = builder.header("Content-Range", range);
        }
        if let Some(len) = content_length {
            if len > 0 {
                builder = builder.header("Content-Length", len.to_string());
            }
        }

        // No body for HEAD, 304 Not Modified or 412 Precondition Failed.
        let empty_body = head
            || status == StatusCode::NOT_MODIFIED
            || status == StatusCode::PRECONDITION_FAILED;
        if empty_body {
            return builder.body(empty()).unwrap();
        }
        builder.body(util::stream(resp.bytes_stream())).unwrap()
    }

    fn directory_listing(&self, info: &FileInfo) -> Response<Body> {
        let mut html = String::with_capacity(256);
        html.push_str("<!DOCTYPE html>\n<html><head><title>Index of ");
        html.push_str(&escape(&info.path));
        html.push_str("</title></head><body>\n<h1>Index of ");
        html.push_str(&escape(&info.path));
        html.push_str("</h1>\n<hr>\n<pre>\n");

        if info.path != "/" {
            html.push_str("<a href=\"../\">../</a>\n");
        }

        for item in &info.items {
            let mut name = item.name.clone();
            if item.is_dir {
                name.push('/');
            }
            html.push_str(&format!(
                "<a href=\"{}\">{}</a>\n",
                util::encode_segment(&name),
                escape(&name)
            ));
        }

        html.push_str("</pre>\n<hr>\n</body></html>\n");

        Response::builder()
            .status(StatusCode::OK)
            .header("Content-Type", "text/html; charset=utf-8")
            .body(full(html))
            .unwrap()
    }

    async fn put(&self, req: Request<Incoming>, auth: &Auth, path: &str) -> Response<Body> {
        let exists = self.client.get_file_info(auth, path).await.is_ok();
        let body = request_body(req);

        let result = if exists {
            self.client.update_file(auth, path, body).await
        } else {
            self.client.upload_file(auth, path, body, true).await
        };

        if let Err(err) = result {
            return self.error(err);
        }

        if exists {
            status(StatusCode::NO_CONTENT)
        } else {
            status(StatusCode::CREATED)
        }
    }

    async fn delete(&self, auth: &Auth, path: &str) -> Response<Body> {
        match self.client.delete(auth, path).await {
            Ok(()) => status(StatusCode::NO_CONTENT),
            Err(err) => self.error(err),
        }
    }

    async fn mkcol(
        &self,
        req: &Request<Incoming>,
        auth: &Auth,
        path: &str,
    ) -> Response<Body> {
        if content_length(req) > 0 {
            return text(
                StatusCode::UNSUPPORTED_MEDIA_TYPE,
                "MKCOL with body not supported",
            );
        }

        // RFC 4918: MKCOL on an existing resource is 405; a missing parent is 409.
        match self.client.get_file_info(auth, path).await {
            Ok(_) => {
                return Response::builder()
                    .status(StatusCode::METHOD_NOT_ALLOWED)
                    .header(
                        "Allow",
                        "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, COPY, MOVE, LOCK, UNLOCK",
                    )
                    .header("Content-Type", "text/plain; charset=utf-8")
                    .body(full("Resource already exists\n"))
                    .unwrap();
            }
            Err(client::Error::NotFound) => {}
            Err(err) => return self.error(err),
        }

        if let Some(parent) = parent_path(path) {
            match self.client.get_file_info(auth, &parent).await {
                Ok(info) if !info.is_dir => return text(StatusCode::CONFLICT, "Parent is not a collection"),
                Ok(_) => {}
                Err(client::Error::NotFound) => {
                    return text(StatusCode::CONFLICT, "Parent collection does not exist")
                }
                Err(err) => return self.error(err),
            }
        }

        match self.client.create_directory(auth, path).await {
            Ok(()) => status(StatusCode::CREATED),
            Err(err) => self.error(err),
        }
    }

    async fn copy_or_move(
        &self,
        req: &Request<Incoming>,
        auth: &Auth,
        path: &str,
        is_move: bool,
    ) -> Response<Body> {
        let dst = match self.destination(req) {
            Some(dst) => dst,
            None => return text(StatusCode::BAD_REQUEST, "Destination header required"),
        };

        let overwrite = req
            .headers()
            .get("Overwrite")
            .and_then(|v| v.to_str().ok())
            != Some("F");

        let dst_exists = self.client.get_file_info(auth, &dst).await.is_ok();

        let result = if is_move {
            self.client.rename(auth, path, &dst, overwrite).await
        } else {
            self.client.copy(auth, path, &dst, overwrite).await
        };

        if let Err(err) = result {
            return self.error(err);
        }

        if dst_exists && overwrite {
            status(StatusCode::NO_CONTENT)
        } else {
            status(StatusCode::CREATED)
        }
    }

    fn destination(&self, req: &Request<Incoming>) -> Option<String> {
        let raw = req.headers().get("Destination")?.to_str().ok()?;
        if raw.is_empty() {
            return None;
        }

        // Accept either an absolute URL or a bare path.
        let rest = match raw.find("://") {
            Some(idx) => {
                let after = &raw[idx + 3..];
                match after.find('/') {
                    Some(slash) => &after[slash..],
                    None => "",
                }
            }
            None => raw,
        };
        let rest = rest.split(['?', '#']).next().unwrap_or("");

        let decoded = percent_decode_str(rest).decode_utf8_lossy();
        let stripped = self.strip_prefix(&decoded);
        let trimmed = stripped.trim_end_matches('/').to_string();

        if trimmed.is_empty() {
            None
        } else {
            Some(trimmed)
        }
    }

    async fn lock(&self, auth: &Auth, path: &str) -> Response<Body> {
        match self.client.get_file_info(auth, path).await {
            Ok(_) | Err(client::Error::NotFound) => {}
            Err(err) => return self.error(err),
        }

        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        let token = format!("opaquelocktoken:{nanos:x}");
        let href = format!("{}{}", self.prefix, encode_path(path));

        let mut xml = String::with_capacity(512);
        xml.push_str(XML_HEADER);
        xml.push_str("<D:prop xmlns:D=\"DAV:\">\n");
        xml.push_str("  <D:lockdiscovery>\n    <D:activelock>\n");
        xml.push_str("      <D:locktype><D:write></D:write></D:locktype>\n");
        xml.push_str("      <D:lockscope><D:exclusive></D:exclusive></D:lockscope>\n");
        xml.push_str("      <D:depth>infinity</D:depth>\n");
        xml.push_str(&format!(
            "      <D:owner><D:href>{}</D:href></D:owner>\n",
            escape(&auth.username)
        ));
        xml.push_str("      <D:timeout>Second-3600</D:timeout>\n");
        xml.push_str(&format!(
            "      <D:locktoken><D:href>{}</D:href></D:locktoken>\n",
            escape(&token)
        ));
        xml.push_str(&format!(
            "      <D:lockroot><D:href>{}</D:href></D:lockroot>\n",
            escape(&href)
        ));
        xml.push_str("    </D:activelock>\n  </D:lockdiscovery>\n</D:prop>\n");

        Response::builder()
            .status(StatusCode::OK)
            .header("Content-Type", "application/xml; charset=utf-8")
            .header("Lock-Token", format!("<{token}>"))
            .body(full(xml))
            .unwrap()
    }

    async fn proppatch(&self, auth: &Auth, path: &str) -> Response<Body> {
        if let Err(err) = self.client.get_file_info(auth, path).await {
            return self.error(err);
        }

        let href = format!("{}{}", self.prefix, encode_path(path));
        let mut xml = String::with_capacity(256);
        xml.push_str(XML_HEADER);
        xml.push_str("<D:multistatus xmlns:D=\"DAV:\">\n");
        xml.push_str("  <D:response>\n");
        xml.push_str(&format!("    <D:href>{}</D:href>\n", escape(&href)));
        xml.push_str("    <D:propstat>\n      <D:prop></D:prop>\n");
        xml.push_str("      <D:status>HTTP/1.1 200 OK</D:status>\n");
        xml.push_str("    </D:propstat>\n  </D:response>\n</D:multistatus>\n");

        xml_response(StatusCode::MULTI_STATUS, xml)
    }

    fn error(&self, err: client::Error) -> Response<Body> {
        if self.debug {
            log(LOG_PREFIX, &format!("Error: {err}"));
        }

        match err {
            client::Error::Unauthorized => self.require_auth(),
            client::Error::NotFound => text(StatusCode::NOT_FOUND, "Not Found"),
            client::Error::Forbidden => text(StatusCode::FORBIDDEN, "Forbidden"),
            client::Error::Conflict => text(StatusCode::CONFLICT, "Conflict"),
            client::Error::BadRequest => text(StatusCode::BAD_REQUEST, "Bad Request"),
            client::Error::Other(msg) => {
                log(LOG_PREFIX, &format!("Internal error: {msg}"));
                text(StatusCode::INTERNAL_SERVER_ERROR, "Internal Server Error")
            }
        }
    }
}

fn join_path(base: &str, name: &str) -> String {
    if base.ends_with('/') {
        format!("{base}{name}")
    } else {
        format!("{base}/{name}")
    }
}

/// Returns the parent collection path, or `None` for the root.
fn parent_path(path: &str) -> Option<String> {
    let trimmed = path.trim_end_matches('/');
    let idx = trimmed.rfind('/')?;
    if idx == 0 {
        Some("/".to_string())
    } else {
        Some(trimmed[..idx].to_string())
    }
}

/// Reads a single response header as an owned string, if present and valid UTF-8.
fn header_value(resp: &reqwest::Response, name: &str) -> Option<String> {
    resp.headers()
        .get(name)
        .and_then(|v| v.to_str().ok())
        .map(str::to_string)
}

fn request_body(req: Request<Incoming>) -> reqwest::Body {
    let stream = BodyStream::new(req.into_body()).filter_map(|frame| async move {
        match frame {
            Ok(frame) => frame.into_data().ok().map(Ok),
            Err(err) => Some(Err(err)),
        }
    });
    reqwest::Body::wrap_stream(stream)
}

fn content_length(req: &Request<Incoming>) -> u64 {
    req.headers()
        .get("Content-Length")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.parse().ok())
        .unwrap_or(0)
}

fn etag(info: &FileInfo) -> String {
    format!("\"{:x}-{:x}\"", info.modified.unix_timestamp(), info.size)
}

fn http_date(time: OffsetDateTime) -> String {
    httpdate::fmt_http_date(SystemTime::from(time))
}

fn rfc3339(time: OffsetDateTime) -> String {
    // Whole-second precision only: some WebDAV clients (notably the Windows
    // Mini-Redirector) reject fractional seconds in `creationdate`.
    time.to_offset(UtcOffset::UTC)
        .replace_nanosecond(0)
        .unwrap_or(time)
        .format(&Rfc3339)
        .unwrap_or_default()
}

fn mime_type(name: &str, file_type: &str) -> &'static str {
    let ext = || {
        name.rsplit_once('.')
            .map(|(_, e)| e.to_ascii_lowercase())
            .unwrap_or_default()
    };

    match file_type {
        "text" | "textImmutable" => "text/plain; charset=utf-8",
        "image" => match ext().as_str() {
            "jpg" | "jpeg" => "image/jpeg",
            "png" => "image/png",
            "gif" => "image/gif",
            "webp" => "image/webp",
            "svg" => "image/svg+xml",
            _ => "image/jpeg",
        },
        "video" => match ext().as_str() {
            "mp4" => "video/mp4",
            "webm" => "video/webm",
            "mkv" => "video/x-matroska",
            "avi" => "video/x-msvideo",
            _ => "video/mp4",
        },
        "audio" => match ext().as_str() {
            "mp3" => "audio/mpeg",
            "wav" => "audio/wav",
            "ogg" => "audio/ogg",
            "flac" => "audio/flac",
            _ => "audio/mpeg",
        },
        "pdf" => "application/pdf",
        _ => "application/octet-stream",
    }
}

fn status(code: StatusCode) -> Response<Body> {
    Response::builder().status(code).body(empty()).unwrap()
}

fn text(code: StatusCode, message: &str) -> Response<Body> {
    Response::builder()
        .status(code)
        .header("Content-Type", "text/plain; charset=utf-8")
        .header("X-Content-Type-Options", "nosniff")
        .body(full(format!("{message}\n")))
        .unwrap()
}

fn xml_response(code: StatusCode, body: String) -> Response<Body> {
    Response::builder()
        .status(code)
        .header("Content-Type", "application/xml; charset=utf-8")
        .body(full(body))
        .unwrap()
}
