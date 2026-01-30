//! Shared helpers: path encoding, XML/HTML escaping, response bodies and logging.

use bytes::Bytes;
use futures_util::{Stream, StreamExt};
use http_body_util::combinators::BoxBody;
use http_body_util::{BodyExt, Empty, Full, StreamBody};
use hyper::body::Frame;
use percent_encoding::{utf8_percent_encode, AsciiSet, NON_ALPHANUMERIC};
use std::io;
use time::macros::format_description;
use time::OffsetDateTime;

/// Response body type used throughout the server.
pub type Body = BoxBody<Bytes, io::Error>;

/// Characters left unescaped within a single URL path segment, matching the
/// set permitted by Go's `url.PathEscape`.
const SEGMENT: &AsciiSet = &NON_ALPHANUMERIC
    .remove(b'-')
    .remove(b'_')
    .remove(b'.')
    .remove(b'~')
    .remove(b'$')
    .remove(b'&')
    .remove(b'+')
    .remove(b':')
    .remove(b'=')
    .remove(b'@');

/// Percent-encodes a single path segment.
pub fn encode_segment(segment: &str) -> String {
    utf8_percent_encode(segment, SEGMENT).to_string()
}

/// Percent-encodes each segment of a path, preserving the separators.
pub fn encode_path(path: &str) -> String {
    let owned;
    let path = if path.starts_with('/') {
        path
    } else {
        owned = format!("/{path}");
        &owned
    };

    let mut out = String::with_capacity(path.len());
    for (i, segment) in path.split('/').enumerate() {
        if i > 0 {
            out.push('/');
        }
        out.extend(utf8_percent_encode(segment, SEGMENT));
    }
    out
}

/// Escapes text for safe inclusion in XML/HTML content.
pub fn escape(input: &str) -> String {
    let mut out = String::with_capacity(input.len());
    for c in input.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&apos;"),
            _ => out.push(c),
        }
    }
    out
}

/// Builds a body from owned bytes.
pub fn full<T: Into<Bytes>>(chunk: T) -> Body {
    Full::new(chunk.into())
        .map_err(|never| match never {})
        .boxed()
}

/// Builds an empty body.
pub fn empty() -> Body {
    Empty::<Bytes>::new().map_err(|never| match never {}).boxed()
}

/// Builds a streaming body from a stream of byte chunks.
pub fn stream<S>(source: S) -> Body
where
    S: Stream<Item = reqwest::Result<Bytes>> + Send + Sync + 'static,
{
    let frames = source.map(|chunk| {
        chunk
            .map(Frame::data)
            .map_err(|e| io::Error::new(io::ErrorKind::Other, e))
    });
    BodyExt::boxed(StreamBody::new(frames))
}

/// Writes a timestamped line to stderr, mimicking the standard logger layout.
pub fn log(prefix: &str, message: &str) {
    const FORMAT: &[time::format_description::FormatItem<'_>] =
        format_description!("[year]/[month]/[day] [hour]:[minute]:[second]");
    let ts = OffsetDateTime::now_utc()
        .format(FORMAT)
        .unwrap_or_default();
    eprintln!("{ts} {prefix}{message}");
}
