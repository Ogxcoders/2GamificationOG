#!/bin/bash
# Installs Go 1.23.4 and Rust (stable) into user-writable locations, detached from the session.
# Logs to /tmp/toolchain-install.log

LOG=/tmp/toolchain-install.log
exec > "$LOG" 2>&1

echo "[$(date)] === toolchain install start ==="

# ---------- GO ----------
GO_DIR="$HOME/toolchains/go1.23.4"
if [ ! -x "$GO_DIR/bin/go" ]; then
  echo "[$(date)] downloading go1.23.4.linux-amd64.tar.gz"
  mkdir -p "$HOME/toolchains" "$HOME/dl"
  curl -fsSL -o "$HOME/dl/go1.23.4.linux-amd64.tar.gz" https://go.dev/dl/go1.23.4.linux-amd64.tar.gz \
    || curl -fsSL -o "$HOME/dl/go1.23.4.linux-amd64.tar.gz" https://dl.google.com/go/go1.23.4.linux-amd64.tar.gz
  echo "[$(date)] go tarball size: $(du -h $HOME/dl/go1.23.4.linux-amd64.tar.gz | cut -f1)"
  mkdir -p "$GO_DIR"
  tar -C "$GO_DIR" --strip-components=1 -xzf "$HOME/dl/go1.23.4.linux-amd64.tar.gz"
  echo "[$(date)] go extracted"
fi
"$GO_DIR/bin/go" version && echo "GO_OK"

# ---------- RUST ----------
export RUSTUP_HOME="$HOME/toolchains/rustup"
export CARGO_HOME="$HOME/toolchains/cargo"
if [ ! -x "$CARGO_HOME/bin/cargo" ]; then
  echo "[$(date)] installing rustup (stable, minimal profile)"
  curl -fsSL https://sh.rustup.rs -o "$HOME/dl/rustup-init.sh"
  sh "$HOME/dl/rustup-init.sh" -y --default-toolchain stable --profile minimal --no-modify-path
  echo "[$(date)] rustup done"
fi
"$CARGO_HOME/bin/cargo" --version && echo "RUST_OK"

echo "[$(date)] === toolchain install complete ==="
