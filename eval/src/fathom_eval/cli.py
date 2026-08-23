"""Command line for the harness.

The same entry point serves two very different callers: a nightly CronJob in the cluster
and a person on a workstation iterating on a fixture. Everything is therefore a flag as
well as an environment variable, and the flags win -- but they are re-validated through
config.override, so `--iterations 0` fails exactly the way the env var does rather than
sneaking past the checks.
"""

from __future__ import annotations

import argparse
import logging
import sys

from . import __version__, config, store
from . import judge as judge_mod
from . import serve as serve_mod
from .cluster import Cluster, connect
from .fixtures import load_all
from .runner import run as run_eval


def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="fathom-eval", description=__doc__)
    p.add_argument("--version", action="version", version=__version__)
    sub = p.add_subparsers(dest="command", required=True)

    r = sub.add_parser("run", help="stage fixtures, ask the agent, grade and store")
    r.add_argument("--agent-url")
    r.add_argument("--db", dest="db_path")
    r.add_argument("--iterations", type=int)
    r.add_argument("--fixtures", help="comma-separated subset, for iterating locally")
    r.add_argument("--kubeconfig", dest="kubeconfig")
    r.add_argument("--prometheus-url", dest="prometheus_url")
    r.add_argument(
        "--keep-namespaces",
        action="store_true",
        default=None,
        help="leave fixtures standing after the run, to inspect them by hand",
    )
    r.add_argument(
        "--deadline",
        type=float,
        default=0.0,
        help="seconds; stop and tear down cleanly before the Job's hard deadline",
    )

    j = sub.add_parser("judge", help="score stored answers with the CPU judge model")
    j.add_argument("--db", dest="db_path")
    j.add_argument("--judge-url")
    j.add_argument("--judge-model")
    j.add_argument("--limit", type=int, help="stop after this many judgements")
    j.add_argument(
        "--sample",
        type=int,
        help="per fixture+question; 0 judges everything (slow: minutes each)",
    )

    v = sub.add_parser("serve", help="serve the results database read-only over HTTP")
    v.add_argument("--db", dest="db_path")
    v.add_argument("--addr", dest="serve_addr", help="host:port, e.g. :8080")
    v.add_argument("--max-rows", dest="serve_max_rows", type=int)

    sub.add_parser("fixtures", help="list the packaged fixtures and exit")

    s = sub.add_parser("sweep", help="delete leftover fixture namespaces and exit")
    s.add_argument("--kubeconfig", dest="kubeconfig")

    sub.add_parser("schema", help="print the SQL schema and exit")
    return p


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)

    if args.command == "schema":
        print(store.SCHEMA)
        return 0

    cfg = config.load()
    logging.basicConfig(
        level=cfg.log_level,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
        stream=sys.stdout,
    )

    if args.command == "fixtures":
        for f in load_all():
            marker = "EXCLUSIVE" if f.exclusive else "shared   "
            print(f"{f.order:3d}  {marker}  {f.name:22s}  {f.namespace}")
            for q in f.questions:
                print(f"                          {q.id}: {q.text.strip()[:70]}")
        return 0

    if args.command == "sweep":
        cluster = Cluster(api=connect(args.kubeconfig or cfg.kubeconfig))
        swept = cluster.sweep()
        print(f"swept {len(swept)} namespace(s): {', '.join(swept) or 'none'}")
        return 0

    if args.command == "serve":
        cfg = config.override(
            cfg,
            db_path=args.db_path,
            serve_addr=args.serve_addr,
            serve_max_rows=args.serve_max_rows,
        )
        return serve_mod.serve(cfg)

    if args.command == "judge":
        cfg = config.override(
            cfg, db_path=args.db_path, judge_url=args.judge_url, judge_model=args.judge_model
        )
        return judge_mod.judge(cfg, limit=args.limit, sample=args.sample)

    # run
    cfg = config.override(
        cfg,
        agent_url=args.agent_url,
        db_path=args.db_path,
        iterations=args.iterations,
        kubeconfig=args.kubeconfig,
        prometheus_url=args.prometheus_url,
        keep_namespaces=args.keep_namespaces,
        fixtures=tuple(f.strip() for f in args.fixtures.split(",")) if args.fixtures else None,
    )
    fixtures = load_all(cfg.fixtures)
    if not fixtures:
        print("no fixtures selected", file=sys.stderr)
        return 1
    return run_eval(cfg, fixtures, deadline_seconds=args.deadline)
