"""Prebuilt ``cango`` (codeanalyzer-go) backend binary for CLDK.

This package carries the platform-specific, self-contained ``cango`` executable
(built from this repo with ``go build``) and exposes its filesystem path. CLDK's
Python SDK depends on this package and calls :func:`bin_path` to locate the
analyzer, exactly as it imports ``codeanalyzer-python`` for the Python backend.

Each published wheel is platform-tagged and contains the single binary for that
platform; pip resolves the correct wheel at install time.
"""

from __future__ import annotations

import os
import stat
import sys
from importlib import resources
from pathlib import Path

__version__ = "0.1.0"

__all__ = ["bin_path", "__version__"]

_BINARY_NAME = "cango" + (".exe" if sys.platform == "win32" else "")


def bin_path() -> Path:
    """Return the absolute path to the bundled ``cango`` binary.

    Raises:
        FileNotFoundError: if the wheel for this platform did not include a binary
            (e.g. an unsupported platform, or a source/dev install with no build run).
    """
    resource = resources.files("codeanalyzer_go") / "_bin" / _BINARY_NAME
    with resources.as_file(resource) as extracted:
        path = Path(extracted)

    if not path.exists():
        raise FileNotFoundError(
            f"Bundled cango binary not found at {path}. "
            "This usually means there is no prebuilt wheel for your platform; "
            "build the binary with `go build -o cango ./cmd/codeanalyzer` and point "
            "CLDK at it via analysis_backend_path or by placing it on PATH."
        )

    # Wheels may not preserve the executable bit on POSIX; restore it best-effort.
    if os.name == "posix":
        mode = path.stat().st_mode
        path.chmod(mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)

    return path
