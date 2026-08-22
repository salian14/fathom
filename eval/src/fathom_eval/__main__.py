"""Entry point: `python -m fathom_eval`."""

from __future__ import annotations

import sys

from . import config, telemetry
from .cli import main

if __name__ == "__main__":
    # Telemetry is configured here rather than in cli.main so that importing the CLI for
    # a test never installs a global provider.
    try:
        cfg = config.load()
        telemetry.configure(cfg.telemetry)
    except Exception:  # noqa: BLE001 - a bad telemetry setting must not block a run
        pass
    try:
        sys.exit(main())
    finally:
        telemetry.shutdown()
