//! Server configuration, request routing and the connection accept loop.

use std::env;
use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use hyper::server::conn::http1;
use hyper::service::service_fn;
use hyper::{Request, Response, StatusCode};
use hyper_util::rt::TokioIo;
use tokio::net::TcpListener;

use crate::client::Client;
use crate::util::{empty, full, log, Body};
use crate::webdav::Handler;

/// Runtime configuration assembled from environment variables and CLI flags.
pub struct Config {
    pub filebrowser_url: String,
    pub listen: String,
    pub prefix: String,
    pub debug: bool,
    pub token_cache_ttl_secs: u64,
}

impl Config {
    /// Builds configuration from environment variables, applying defaults.
    pub fn from_env() -> Self {
        Config {
            filebrowser_url: env_or("FILEBROWSER_URL", "http://localhost:8080"),
            listen: env_or("LISTEN_ADDR", ":8081"),
            prefix: env_or("WEBDAV_PREFIX", "/"),
            debug: env_bool("DEBUG", false),
            token_cache_ttl_secs: env_u64("TOKEN_CACHE_TTL", 1800),
        }
    }
}

/// Shared application state for the routing layer.
pub struct App {
    handler: Handler,
    root_mode: bool,
    mount: String,
    mount_slash: String,
    debug: bool,
}

impl App {
    /// Creates application state from configuration.
    pub fn new(config: &Config) -> Arc<App> {
        let base = config.filebrowser_url.trim_end_matches('/').to_string();
        let client = Arc::new(Client::new(
            base,
            Duration::from_secs(config.token_cache_ttl_secs),
        ));

        let root_mode = config.prefix == "/";
        let mount = config
            .prefix
            .strip_suffix('/')
            .unwrap_or(&config.prefix)
            .to_string();

        let handler = Handler::new(client, mount.clone(), config.debug);

        Arc::new(App {
            handler,
            root_mode,
            mount_slash: format!("{mount}/"),
            mount,
            debug: config.debug,
        })
    }
}

async fn route(app: Arc<App>, req: Request<hyper::body::Incoming>) -> Response<Body> {
    let path = req.uri().path();

    if path == "/health" {
        return Response::builder()
            .status(StatusCode::OK)
            .header("Content-Type", "application/json")
            .body(full("{\"status\":\"OK\"}"))
            .unwrap();
    }

    if app.root_mode {
        return app.handler.serve(req).await;
    }

    if path == app.mount || path.starts_with(&app.mount_slash) {
        app.handler.serve(req).await
    } else if path == "/" {
        Response::builder()
            .status(StatusCode::MOVED_PERMANENTLY)
            .header("Location", app.mount_slash.clone())
            .body(empty())
            .unwrap()
    } else {
        Response::builder()
            .status(StatusCode::NOT_FOUND)
            .header("Content-Type", "text/plain; charset=utf-8")
            .body(full("Not Found\n"))
            .unwrap()
    }
}

/// Accepts connections on the listener and serves them until the task is dropped.
pub async fn serve(listener: TcpListener, app: Arc<App>) {
    loop {
        let (stream, _) = match listener.accept().await {
            Ok(conn) => conn,
            Err(err) => {
                log("", &format!("Accept error: {err}"));
                continue;
            }
        };

        let app = app.clone();
        let debug = app.debug;
        tokio::spawn(async move {
            let io = TokioIo::new(stream);
            let service = service_fn(move |req| {
                let app = app.clone();
                async move { Ok::<_, std::convert::Infallible>(route(app, req).await) }
            });

            if let Err(err) = http1::Builder::new()
                .serve_connection(io, service)
                .await
            {
                if debug {
                    log("", &format!("Connection error: {err}"));
                }
            }
        });
    }
}

/// Parses a listen address, expanding a bare `:port` to all interfaces.
pub fn parse_listen(addr: &str) -> Result<SocketAddr, std::net::AddrParseError> {
    if let Some(port) = addr.strip_prefix(':') {
        format!("0.0.0.0:{port}").parse()
    } else {
        addr.parse()
    }
}

fn env_or(key: &str, default: &str) -> String {
    env::var(key)
        .ok()
        .filter(|v| !v.is_empty())
        .unwrap_or_else(|| default.to_string())
}

fn env_bool(key: &str, default: bool) -> bool {
    match env::var(key) {
        Ok(v) if !v.is_empty() => v.eq_ignore_ascii_case("true") || v == "1",
        _ => default,
    }
}

fn env_u64(key: &str, default: u64) -> u64 {
    env::var(key)
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(default)
}
