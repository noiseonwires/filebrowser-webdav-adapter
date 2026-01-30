# Build stage
FROM rust:1-slim AS builder

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

ENV BUILD_VERSION=${VERSION} \
    BUILD_COMMIT=${COMMIT} \
    BUILD_DATE=${DATE}

WORKDIR /app

COPY Cargo.toml Cargo.lock* ./
COPY src ./src

RUN cargo build --release && strip target/release/webdav-adapter

# Final stage
FROM debian:stable-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /app/target/release/webdav-adapter .

RUN useradd --system --no-create-home appuser
USER appuser

EXPOSE 8081

ENTRYPOINT ["./webdav-adapter"]
