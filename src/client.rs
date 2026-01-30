//! FileBrowser API client with streaming uploads and downloads.

use std::collections::HashMap;
use std::fmt;
use std::sync::RwLock;
use std::time::{Duration, Instant};

use reqwest::StatusCode;
use serde::Deserialize;
use time::OffsetDateTime;

use crate::util::encode_path;

fn epoch() -> OffsetDateTime {
    OffsetDateTime::UNIX_EPOCH
}

/// File or directory metadata returned by FileBrowser.
#[derive(Debug, Clone, Deserialize)]
pub struct FileInfo {
    #[serde(default)]
    pub path: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub size: i64,
    #[serde(default = "epoch", with = "time::serde::rfc3339")]
    pub modified: OffsetDateTime,
    #[serde(default, rename = "isDir")]
    pub is_dir: bool,
    #[serde(default, rename = "type")]
    pub file_type: String,
    #[serde(default)]
    pub items: Vec<FileInfo>,
}

/// Errors surfaced by the FileBrowser client.
#[derive(Debug)]
pub enum Error {
    Unauthorized,
    NotFound,
    Forbidden,
    Conflict,
    BadRequest,
    Other(String),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Unauthorized => write!(f, "unauthorized"),
            Error::NotFound => write!(f, "not found"),
            Error::Forbidden => write!(f, "forbidden"),
            Error::Conflict => write!(f, "conflict"),
            Error::BadRequest => write!(f, "bad request"),
            Error::Other(msg) => write!(f, "{msg}"),
        }
    }
}

impl std::error::Error for Error {}

impl From<reqwest::Error> for Error {
    fn from(err: reqwest::Error) -> Self {
        Error::Other(err.to_string())
    }
}

/// Authentication credentials translated to a FileBrowser JWT token.
#[derive(Clone)]
pub struct Auth {
    pub username: String,
    pub password: String,
}

impl Auth {
    fn cache_key(&self) -> String {
        format!("{}:{}", self.username, self.password)
    }
}

struct TokenEntry {
    token: String,
    expires_at: Instant,
}

/// A FileBrowser API client with a shared connection pool and token cache.
pub struct Client {
    base_url: String,
    http: reqwest::Client,
    token_cache: RwLock<HashMap<String, TokenEntry>>,
    token_cache_ttl: Duration,
}

impl Client {
    /// Creates a new client targeting the given FileBrowser base URL.
    pub fn new(base_url: impl Into<String>, token_cache_ttl: Duration) -> Self {
        let base_url = base_url.into().trim_end_matches('/').to_string();
        Client {
            base_url,
            http: reqwest::Client::new(),
            token_cache: RwLock::new(HashMap::new()),
            token_cache_ttl,
        }
    }

    fn resources_url(&self, path: &str) -> String {
        format!("{}/api/resources{}", self.base_url, encode_path(path))
    }

    /// Authenticates with FileBrowser, caching the resulting token.
    pub async fn login(&self, auth: &Auth) -> Result<String, Error> {
        let key = auth.cache_key();

        if let Ok(cache) = self.token_cache.read() {
            if let Some(entry) = cache.get(&key) {
                if Instant::now() < entry.expires_at {
                    return Ok(entry.token.clone());
                }
            }
        }

        let payload = serde_json::json!({
            "username": auth.username,
            "password": auth.password,
        });

        let resp = self
            .http
            .post(format!("{}/api/login", self.base_url))
            .header("Content-Type", "application/json")
            .body(payload.to_string())
            .send()
            .await?;

        if resp.status() == StatusCode::FORBIDDEN {
            return Err(Error::Unauthorized);
        }
        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            return Err(Error::Other(format!(
                "login failed with status {status}: {body}"
            )));
        }

        let token = resp.text().await?.trim().to_string();

        if let Ok(mut cache) = self.token_cache.write() {
            cache.insert(
                key,
                TokenEntry {
                    token: token.clone(),
                    expires_at: Instant::now() + self.token_cache_ttl,
                },
            );
        }

        Ok(token)
    }

    fn invalidate(&self, auth: &Auth) {
        if let Ok(mut cache) = self.token_cache.write() {
            cache.remove(&auth.cache_key());
        }
    }

    /// Sends an authenticated request, transparently refreshing the token and
    /// retrying once if FileBrowser rejects a cached token with `401`.
    ///
    /// The builder must not carry a non-replayable streaming body, since it is
    /// invoked again on retry.
    async fn send_authed<F>(&self, auth: &Auth, build: F) -> Result<reqwest::Response, Error>
    where
        F: Fn(&reqwest::Client, &str) -> reqwest::RequestBuilder,
    {
        let mut refreshed = false;
        loop {
            let token = self.login(auth).await?;
            let resp = build(&self.http, &token).send().await?;
            if resp.status() == StatusCode::UNAUTHORIZED && !refreshed {
                self.invalidate(auth);
                refreshed = true;
                continue;
            }
            return Ok(resp);
        }
    }

    /// Retrieves file or directory metadata.
    pub async fn get_file_info(&self, auth: &Auth, path: &str) -> Result<FileInfo, Error> {
        let url = self.resources_url(path);
        let resp = self
            .send_authed(auth, |http, token| http.get(url.as_str()).header("X-Auth", token))
            .await?;

        self.check_read(auth, resp.status())?;
        let body = resp.bytes().await?;
        serde_json::from_slice(&body).map_err(|e| Error::Other(format!("decoding response: {e}")))
    }

    /// Streams file content from FileBrowser, forwarding the given request
    /// headers (e.g. `Range`, `If-None-Match`) and returning the raw response
    /// so range and conditional statuses can be propagated to the client.
    pub async fn download_file(
        &self,
        auth: &Auth,
        path: &str,
        forward: &[(&str, String)],
    ) -> Result<reqwest::Response, Error> {
        let url = format!("{}/api/raw{}", self.base_url, encode_path(path));
        let resp = self
            .send_authed(auth, |http, token| {
                let mut rb = http.get(url.as_str()).header("X-Auth", token);
                for (name, value) in forward {
                    rb = rb.header(*name, value);
                }
                rb
            })
            .await?;

        let status = resp.status();
        if status == StatusCode::UNAUTHORIZED {
            self.invalidate(auth);
            return Err(Error::Unauthorized);
        }
        if status == StatusCode::NOT_FOUND {
            return Err(Error::NotFound);
        }
        if status == StatusCode::FORBIDDEN {
            return Err(Error::Forbidden);
        }
        // Allow success plus the range/conditional statuses through untouched.
        let passthrough = status.is_success()
            || matches!(
                status,
                StatusCode::NOT_MODIFIED
                    | StatusCode::PRECONDITION_FAILED
                    | StatusCode::RANGE_NOT_SATISFIABLE
            );
        if !passthrough {
            return Err(status_error(resp).await);
        }
        Ok(resp)
    }

    /// Streams file content to FileBrowser, creating a new resource.
    pub async fn upload_file(
        &self,
        auth: &Auth,
        path: &str,
        body: reqwest::Body,
        overwrite: bool,
    ) -> Result<(), Error> {
        let token = self.login(auth).await?;
        let mut url = self.resources_url(path);
        if overwrite {
            url.push_str("?override=true");
        }

        let resp = self
            .http
            .post(url)
            .header("X-Auth", token)
            .header("Content-Type", "application/octet-stream")
            .body(body)
            .send()
            .await?;

        self.check_write(auth, resp.status()).await
    }

    /// Streams replacement content to an existing file.
    pub async fn update_file(
        &self,
        auth: &Auth,
        path: &str,
        body: reqwest::Body,
    ) -> Result<(), Error> {
        let token = self.login(auth).await?;
        let resp = self
            .http
            .put(self.resources_url(path))
            .header("X-Auth", token)
            .header("Content-Type", "application/octet-stream")
            .body(body)
            .send()
            .await?;

        self.check_write(auth, resp.status()).await
    }

    /// Creates a new directory.
    pub async fn create_directory(&self, auth: &Auth, path: &str) -> Result<(), Error> {
        let mut dir = path.to_string();
        if !dir.ends_with('/') {
            dir.push('/');
        }
        let url = self.resources_url(&dir);

        let resp = self
            .send_authed(auth, |http, token| http.post(url.as_str()).header("X-Auth", token))
            .await?;

        self.check_write(auth, resp.status()).await
    }

    /// Removes a file or directory.
    pub async fn delete(&self, auth: &Auth, path: &str) -> Result<(), Error> {
        let url = self.resources_url(path);
        let resp = self
            .send_authed(auth, |http, token| http.delete(url.as_str()).header("X-Auth", token))
            .await?;

        let status = resp.status();
        if status == StatusCode::UNAUTHORIZED {
            self.invalidate(auth);
            return Err(Error::Unauthorized);
        }
        if status == StatusCode::NOT_FOUND {
            return Err(Error::NotFound);
        }
        if status == StatusCode::FORBIDDEN {
            return Err(Error::Forbidden);
        }
        if !status.is_success() {
            return Err(status_error(resp).await);
        }
        Ok(())
    }

    /// Copies a file or directory to a new location.
    pub async fn copy(
        &self,
        auth: &Auth,
        src: &str,
        dst: &str,
        overwrite: bool,
    ) -> Result<(), Error> {
        self.move_or_copy(auth, src, dst, "copy", overwrite).await
    }

    /// Moves or renames a file or directory.
    pub async fn rename(
        &self,
        auth: &Auth,
        src: &str,
        dst: &str,
        overwrite: bool,
    ) -> Result<(), Error> {
        self.move_or_copy(auth, src, dst, "rename", overwrite).await
    }

    async fn move_or_copy(
        &self,
        auth: &Auth,
        src: &str,
        dst: &str,
        action: &str,
        overwrite: bool,
    ) -> Result<(), Error> {
        let mut query: Vec<(&str, &str)> = vec![("action", action), ("destination", dst)];
        if overwrite {
            query.push(("override", "true"));
        }
        let url = self.resources_url(src);

        let resp = self
            .send_authed(auth, |http, token| {
                http.patch(url.as_str())
                    .header("X-Auth", token)
                    .query(&query)
            })
            .await?;

        let status = resp.status();
        if status == StatusCode::UNAUTHORIZED {
            self.invalidate(auth);
            return Err(Error::Unauthorized);
        }
        match status {
            StatusCode::NOT_FOUND => return Err(Error::NotFound),
            StatusCode::FORBIDDEN => return Err(Error::Forbidden),
            StatusCode::CONFLICT => return Err(Error::Conflict),
            StatusCode::BAD_REQUEST => return Err(Error::BadRequest),
            _ => {}
        }
        if !status.is_success() {
            return Err(status_error(resp).await);
        }
        Ok(())
    }

    fn check_read(&self, auth: &Auth, status: StatusCode) -> Result<(), Error> {
        match status {
            StatusCode::UNAUTHORIZED => {
                self.invalidate(auth);
                Err(Error::Unauthorized)
            }
            StatusCode::NOT_FOUND => Err(Error::NotFound),
            StatusCode::FORBIDDEN => Err(Error::Forbidden),
            s if s.is_success() => Ok(()),
            other => Err(Error::Other(format!("request failed with status {other}"))),
        }
    }

    async fn check_write(&self, auth: &Auth, status: StatusCode) -> Result<(), Error> {
        match status {
            StatusCode::UNAUTHORIZED => {
                self.invalidate(auth);
                Err(Error::Unauthorized)
            }
            StatusCode::NOT_FOUND => Err(Error::NotFound),
            StatusCode::FORBIDDEN => Err(Error::Forbidden),
            StatusCode::CONFLICT => Err(Error::Conflict),
            s if s.is_success() => Ok(()),
            other => Err(Error::Other(format!("request failed with status {other}"))),
        }
    }
}

async fn status_error(resp: reqwest::Response) -> Error {
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    Error::Other(format!("request failed with status {status}: {body}"))
}
