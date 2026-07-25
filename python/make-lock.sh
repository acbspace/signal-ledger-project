#!/bin/sh
# Regenerate the fully pinned, hashed locks (requirements*.txt) from the
# intent files (requirements*.in). Needs Docker and network access.
#
#   sh python/make-lock.sh
#
# Resolution runs inside the same python:3.12-slim base the quant image builds
# on. A lock resolved against a developer's own interpreter can pin a different
# wheel set than the image ends up installing, which would defeat the point of
# locking: the engine stamps ENGINE_VERSION on results whose floating-point
# accumulation depends on the polars and numpy builds underneath it.
set -eu

cd "$(dirname "$0")"

# Git Bash on Windows rewrites container-side paths in -v/-w unless told not to;
# `pwd -W` gives it the Windows-form host path Docker Desktop expects.
MSYS_NO_PATHCONV=1 docker run --rm \
    -v "$(pwd -W 2>/dev/null || pwd):/w" -w /w \
    python:3.12-slim sh -c '
        pip install --no-cache-dir --quiet uv
        for f in requirements requirements-dev; do
            uv pip compile --generate-hashes \
                --custom-compile-command "./make-lock.sh" \
                --output-file "$f.txt" "$f.in"
        done
    '
