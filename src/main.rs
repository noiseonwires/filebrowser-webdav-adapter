use std::process::ExitCode;

use tokio::net::TcpListener;

use webdav_adapter::server::{parse_listen, serve, App, Config};
use webdav_adapter::util::log;

const VERSION: &str = match option_env!("BUILD_VERSION") {
    Some(v) => v,
    None => env!("CARGO_PKG_VERSION"),
};
const COMMIT: &str = match option_env!("BUILD_COMMIT") {
    Some(v) => v,
    None => "none",
};
const DATE: &str = match option_env!("BUILD_DATE") {
    Some(v) => v,
    None => "unknown",
};

#[tokio::main]
async fn main() -> ExitCode {
    let mut config = Config::from_env();

    match parse_args(&mut config) {
        Ok(Action::Run) => {}
        Ok(Action::Version) => {
            println!("webdav-adapter {VERSION} (commit: {COMMIT}, built: {DATE})");
            return ExitCode::SUCCESS;
        }
        Ok(Action::Help) => {
            print_usage();
            return ExitCode::SUCCESS;
        }
        Err(message) => {
            eprintln!("{message}");
            return ExitCode::from(2);
        }
    }

    config.filebrowser_url = config.filebrowser_url.trim_end_matches('/').to_string();

    log("", "FileBrowser WebDAV Adapter starting...");
    log("", &format!("  FileBrowser URL: {}", config.filebrowser_url));
    log("", &format!("  Listen address:  {}", config.listen));
    log("", &format!("  URL prefix:      {}", config.prefix));
    log("", &format!("  Debug mode:      {}", config.debug));
    log(
        "",
        &format!("  Token cache TTL: {}s", config.token_cache_ttl_secs),
    );

    let addr = match parse_listen(&config.listen) {
        Ok(addr) => addr,
        Err(err) => {
            eprintln!("Invalid listen address '{}': {err}", config.listen);
            return ExitCode::FAILURE;
        }
    };

    let listener = match TcpListener::bind(addr).await {
        Ok(listener) => listener,
        Err(err) => {
            eprintln!("Failed to bind {addr}: {err}");
            return ExitCode::FAILURE;
        }
    };

    let app = App::new(&config);
    log("", &format!("WebDAV server listening on {}", config.listen));
    serve(listener, app).await;

    ExitCode::SUCCESS
}

enum Action {
    Run,
    Version,
    Help,
}

fn parse_args(config: &mut Config) -> Result<Action, String> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let mut i = 0;
    while i < args.len() {
        let (key, inline) = match args[i].split_once('=') {
            Some((k, v)) => (k.to_string(), Some(v.to_string())),
            None => (args[i].clone(), None),
        };

        match key.as_str() {
            "--filebrowser-url" => config.filebrowser_url = next_value(&args, &mut i, inline, &key)?,
            "--listen" => config.listen = next_value(&args, &mut i, inline, &key)?,
            "--prefix" => config.prefix = next_value(&args, &mut i, inline, &key)?,
            "--debug" => config.debug = true,
            "--token-cache-ttl" => {
                let value = next_value(&args, &mut i, inline, &key)?;
                config.token_cache_ttl_secs = value
                    .parse()
                    .map_err(|_| format!("invalid value for --token-cache-ttl: {value}"))?;
            }
            "--version" => return Ok(Action::Version),
            "--help" | "-h" => return Ok(Action::Help),
            other => return Err(format!("unknown flag: {other}")),
        }
        i += 1;
    }
    Ok(Action::Run)
}

fn next_value(
    args: &[String],
    i: &mut usize,
    inline: Option<String>,
    key: &str,
) -> Result<String, String> {
    if let Some(value) = inline {
        return Ok(value);
    }
    *i += 1;
    args.get(*i)
        .cloned()
        .ok_or_else(|| format!("missing value for {key}"))
}

fn print_usage() {
    println!(
        "FileBrowser WebDAV Adapter

Usage: webdav-adapter [options]

Options:
  --filebrowser-url <url>   FileBrowser server URL (default http://localhost:8080)
  --listen <addr>           Address to listen on (default :8081)
  --prefix <path>           URL prefix for WebDAV requests (default /)
  --token-cache-ttl <secs>  JWT token cache TTL in seconds (default 1800)
  --debug                   Enable debug logging
  --version                 Show version information
  --help                    Show this help message"
    );
}
