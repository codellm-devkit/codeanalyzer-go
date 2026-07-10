"""Console-script entry point: run the bundled ``cango`` binary.

``pip install codeanalyzer-go`` installs ``cango`` and ``codeanalyzer-go``
launchers (see ``[project.scripts]`` in ``pyproject.toml``) that call :func:`main`.
We locate the platform binary via :func:`bin_path` and hand off the process to it,
passing through every CLI argument and the exit code unchanged.
"""

from __future__ import annotations

import os
import subprocess
import sys

from . import bin_path


def main() -> "int | None":
    """Exec the bundled ``cango`` binary with the current argv (minus argv[0])."""
    binary = str(bin_path())
    args = [binary, *sys.argv[1:]]

    if os.name == "posix":
        # Replace this process with the binary: no extra Python process lingers,
        # and signals/exit codes are handled by cango directly.
        os.execv(binary, args)
        # os.execv never returns on success.

    # Windows has no execv that behaves like POSIX; spawn and propagate the code.
    return subprocess.call(args)


if __name__ == "__main__":
    sys.exit(main())
