---
title: Installation
---

## Install script

Install the latest release into `~/.local/bin` on Linux or macOS.
Supports x86-64 (AMD64) and ARM64 (including Apple Silicon), plus 32-bit x86 on Linux:

```shell
curl -fsSL https://nationallibraryofnorway.github.io/warchaeology/install.sh | bash
```

The script verifies the release checksum. To select another installation directory:

```shell
curl -fsSL https://nationallibraryofnorway.github.io/warchaeology/install.sh \
  | WARC_INSTALL_DIR="$HOME/bin" bash
```

Add the installation directory to your `PATH` if it is not already included.

## Linux

### RPM package

Download the latest `.rpm` from the
[latest release page](https://github.com/nationallibraryofnorway/warchaeology/releases/latest), then install it.

### Debian package

Download the latest `.deb` from the
[latest release page](https://github.com/nationallibraryofnorway/warchaeology/releases/latest), then install it.

### Binary download

The example below uses x86-64. For ARM64, replace `x86_64` with `arm64`.

```shell
curl -LO https://github.com/nationallibraryofnorway/warchaeology/releases/latest/download/warchaeology_Linux_x86_64.tar.gz
tar zxvf warchaeology_Linux_x86_64.tar.gz
sudo install warc /usr/local/bin/warc
```

For a local installation for the current user:

```shell
install warc "$HOME/.local/bin"
```

### Command completion

When using `.deb` or `.rpm` packages, command completion is configured automatically.
For other installation methods, generate completion scripts with:

```shell
warc completion
```

## macOS

The example below uses x86-64. For Apple Silicon, replace `x86_64` with `arm64`.

```shell
curl -LO https://github.com/nationallibraryofnorway/warchaeology/releases/latest/download/warchaeology_Darwin_x86_64.tar.gz
tar zxvf warchaeology_Darwin_x86_64.tar.gz
sudo install warc /usr/local/bin/warc
```

## Docker

Images are published to `ghcr.io/nationallibraryofnorway/warchaeology`.
Use `latest` for the latest stable release, a version such as `5.0.0` for a
specific release, or `main` for the main branch:

```shell
docker run --rm ghcr.io/nationallibraryofnorway/warchaeology:latest --help
```

To build locally, run from the repository root:

```shell
docker build -t warchaeology .
docker run --rm warchaeology --help
```

The image runs `warc` as a non-root user on a distroless base. On Linux, use your
user and group IDs when mounting a writable archive directory:

```shell
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD:/data" -w /data \
  warchaeology index --index-format badger --index-dir ./warcdb capture.wacz

docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD:/data" -w /data -p 9999:9999 \
  warchaeology serve --index-dir ./warcdb capture.wacz
```

The mounted directory retains the index database between runs. File hooks that
invoke external programs require those programs to be added to a custom image.
