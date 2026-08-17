"""Entrypoint: `python -m fathom_agent`.

Config errors are fatal and printed in the same shape as the Go component's
(`fathom-agent: <error>` on stderr, exit 1), so a bad value is legible from the first
line of `kubectl logs` rather than a traceback.
"""

from __future__ import annotations

import sys

from .api import main

if __name__ == "__main__":
    try:
        main()
    except ValueError as err:
        print(f"fathom-agent: {err}", file=sys.stderr)
        sys.exit(1)
