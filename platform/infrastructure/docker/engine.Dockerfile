# engine-service (Rust) — multi-stage build.
FROM rust:1.83-slim AS builder
WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY crates ./crates
RUN cargo build --release --bin engine-service

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates wget && rm -rf /var/lib/apt/lists/*
COPY --from=builder /src/target/release/engine-service /usr/local/bin/engine-service
ENV ENGINE_BIND=0.0.0.0:8081 RUST_LOG=info
EXPOSE 8081
USER 1000
ENTRYPOINT ["engine-service"]
