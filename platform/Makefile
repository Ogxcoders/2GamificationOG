# Universal Engagement Platform — monorepo makefile.
SHELL := /bin/bash
export PATH := $(HOME)/toolchains/cargo/bin:$(HOME)/toolchains/go1.23.4/bin:$(PATH)
export RUSTUP_HOME := $(HOME)/toolchains/rustup
export CARGO_HOME := $(HOME)/toolchains/cargo

CP := services/control-plane

.PHONY: build build-rust build-go build-admin test test-rust vet fmt clean run compose bootstrap e2e

## build: everything (rust engine, go control plane, admin console)
build: build-rust build-go build-admin

build-rust:
	cd platform 2>/dev/null || true
	cargo build --release --manifest-path Cargo.toml

build-go:
	cd $(CP) && go build ./...
	cd $(CP) && go build -o bin/control-plane ./cmd/control-plane

build-admin:
	cd apps/admin && npm install --no-audit --no-fund && npx vite build

## test: rust unit tests (93) + go vet
test: test-rust vet
test-rust:
	cargo test --manifest-path Cargo.toml

vet:
	cd $(CP) && go vet ./...

fmt:
	cd $(CP) && gofmt -w .
	cargo fmt --manifest-path Cargo.toml --allow-dirty || true

## run: local dev processes (needs postgres on :5432)
run: build-rust build-go
	$(ENGINE_BIN) & cargo run --release --bin engine-service & \
	cd $(CP) && DATABASE_URL=postgres://postgres:postgres@localhost:5432/platform?sslmode=disable ENGINE_URL=http://127.0.0.1:8081 go run ./cmd/control-plane

## compose: docker stack (postgres+redis+engine+control-plane+admin)
compose:
	docker compose -f infrastructure/docker/docker-compose.yml up -d --build

## bootstrap: create the first tenant (run AFTER the stack is up)
bootstrap:
	./scripts/bootstrap.sh

## e2e: golden-path verification against a running stack
e2e:
	./scripts/e2e_golden_path.sh

clean:
	cargo clean 2>/dev/null || true
	cd $(CP) && rm -rf bin
	rm -rf apps/admin/dist apps/admin/node_modules
