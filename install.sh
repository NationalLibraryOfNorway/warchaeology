#!/usr/bin/env bash
# Copyright 2026 National Library of Norway.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

main() {
    local os arch archive release base tmp checksum actual cleanup
    local install_dir="${WARC_INSTALL_DIR:-$HOME/.local/bin}"
    base=https://github.com/nationallibraryofnorway/warchaeology/releases

    case "$(uname -s)" in
        Linux) os=Linux ;;
        Darwin) os=Darwin ;;
        *) echo "Unsupported OS; see the installation guide." >&2; return 1 ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) arch=x86_64 ;;
        arm64|aarch64) arch=arm64 ;;
        i386|i486|i586|i686) arch=i386 ;;
        *) echo "No release binary is available for this architecture." >&2; return 1 ;;
    esac
    if [[ "$os" == Darwin && "$arch" == i386 ]]; then
        echo "No release binary is available for this platform." >&2
        return 1
    fi

    archive="warchaeology_${os}_${arch}.tar.gz"
    release=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$base/latest")
    if [[ "$release" != "$base/tag/"* ]]; then
        echo "Could not resolve the latest release." >&2
        return 1
    fi
    base="$base/download/${release##*/}"
    tmp=$(mktemp -d)
    printf -v cleanup 'rm -rf -- %q' "$tmp"
    # Capture the quoted path now; local variables are gone when EXIT runs.
    # shellcheck disable=SC2064
    trap "$cleanup" EXIT
    curl -fsSL "$base/$archive" -o "$tmp/$archive"
    curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
    checksum=$(awk -v name="$archive" '$2 == name {print $1}' "$tmp/checksums.txt")
    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$tmp/$archive")
    else
        actual=$(shasum -a 256 "$tmp/$archive")
    fi
    if [[ -z "$checksum" || "${actual%% *}" != "$checksum" ]]; then
        echo "Release checksum verification failed." >&2
        return 1
    fi

    tar -xzf "$tmp/$archive" -C "$tmp" warc
    mkdir -p "$install_dir"
    install -m 755 "$tmp/warc" "$install_dir/warc"
    echo "Installed warc in $install_dir"
    case ":$PATH:" in
        *":$install_dir:"*) ;;
        *)
            # Print a literal $PATH for the user's shell configuration.
            # shellcheck disable=SC2016
            printf 'Add this directory to PATH: export PATH="%s:$PATH"\n' "$install_dir"
            ;;
    esac
    rm -rf -- "$tmp"
    trap - EXIT
}

main "$@"
