"""The LLM-judge pass: slow, deferred, and deliberately kept away from the GPU.

The judge runs against a *second* Ollama instance, CPU-only, on port 11435. The RTX 2070
holds 8 GB and qwen3.5:9b at num_ctx=16384 already fills it; with
OLLAMA_MAX_LOADED_MODELS=1 a second model would evict the first, which is exactly what
OLLAMA_KEEP_ALIVE=30m exists to prevent. Pinning the judge to the CPU means it can run
without touching the agent's residency at all -- at roughly 1.5-2.5 tok/s for a 31B on
this hardware, so minutes per judgment rather than seconds. Hence: a separate pass, a
separate CronJob, and sampling by default.

What the judge is *not* allowed to do:

- It never overrides a deterministic check. Its verdict is stored beside them, never
  merged into them. The deterministic checks are the regression signal; this is a second
  opinion on answer quality.
- It never reads the cluster. By the time it runs the fixture namespace is long gone,
  and a judge that looked at live state would be grading a different world than the
  agent saw. The prompt is assembled purely from stored artifacts.
"""

from __future__ import annotations

import hashlib
import json
import logging
import re
from importlib import resources

import httpx

from . import store
from .config import Config
from .fixtures import load_all

log = logging.getLogger("fathom_eval.judge")

RUBRIC_FILE = "answer_v1.md"


def rubric() -> tuple[str, str]:
    """The rubric text and a short hash of it.

    Stored per judgement so a rubric edit produces new rows rather than silently
    changing what old scores meant -- and so the judge re-runs exactly the work that the
    edit invalidated, and nothing else.
    """
    text = (resources.files("fathom_eval") / "rubrics" / RUBRIC_FILE).read_text()
    return text, RUBRIC_FILE.removesuffix(".md") + "-" + hashlib.sha256(text.encode()).hexdigest()[
        :8
    ]


def build_prompt(
    rubric_text: str, ground_truth: str, question: str, answer: str, steps: list
) -> str:
    """Assemble a judging prompt deterministically.

    Sorted keys and a fixed shape so the same result always produces the same prompt --
    otherwise re-judging would silently be a different experiment. Tool *outputs* are
    not available (the /chat response carries only sizes), which is why the ground-truth
    description exists: it is what lets the judge check the answer against reality.
    """
    rendered = (
        "\n".join(
            f"  {i + 1}. {s.get('tool')}({json.dumps(s.get('args') or {}, sort_keys=True)}) "
            f"-> {s.get('status', 'unknown')}, {s.get('result_chars', 0)} chars"
            for i, s in enumerate(steps)
        )
        or "  (no tools were called)"
    )

    return (
        f"{rubric_text}\n\n"
        f"--- GROUND TRUTH (what is actually wrong) ---\n{ground_truth}\n\n"
        f"--- QUESTION ASKED ---\n{question}\n\n"
        f"--- TOOL CALLS THE ASSISTANT MADE ---\n{rendered}\n\n"
        f"--- THE ASSISTANT'S ANSWER ---\n{answer}\n\n"
        f"--- YOUR VERDICT (JSON only) ---\n"
    )


def parse_verdict(raw: str) -> dict:
    """Pull the JSON verdict out of the model's reply.

    Never coerces: a reply that cannot be parsed is recorded as unparseable with the raw
    text kept. A judge that silently defaults to "fail" on a parse error would quietly
    invent a regression.
    """
    text = raw.strip()
    if match := re.search(r"\{.*\}", text, re.DOTALL):
        text = match.group(0)
    parsed = json.loads(text)
    verdict = str(parsed.get("verdict", "")).lower()
    if verdict not in ("pass", "fail"):
        raise ValueError(f"verdict must be pass or fail, got {verdict!r}")
    score = int(parsed.get("score", 0))
    if not 1 <= score <= 5:
        raise ValueError(f"score must be 1-5, got {score}")
    return {"verdict": verdict, "score": score, "rationale": str(parsed.get("rationale", ""))}


def judge(cfg: Config, limit: int | None = None, sample: int | None = None) -> int:
    rubric_text, rubric_version = rubric()
    ground_truth = {f.name: f.description for f in load_all()}
    per_group = cfg.judge_sample if sample is None else sample

    with store.opened(cfg.db_path) as conn:
        # Never judge while a run is in flight: the judge saturates host CPU, the three
        # VMs already pin 12 of 20 threads, and a slowed agent would inflate
        # elapsed_seconds on the very run being recorded. That is measurement error, not
        # a scheduling inconvenience.
        if conn.execute("SELECT 1 FROM runs WHERE status='running' LIMIT 1").fetchone():
            log.warning("a run is still in progress -- not judging; try again later")
            return 0

        rows = store.pending_judgements(conn, rubric_version, per_group, limit)
        if not rows:
            log.info("nothing to judge at rubric %s", rubric_version)
            return 0

        log.info(
            "judging %d result(s) with %s at rubric %s", len(rows), cfg.judge_model, rubric_version
        )
        client = httpx.Client(timeout=cfg.judge_timeout)
        try:
            for row in rows:
                _judge_one(conn, client, cfg, row, rubric_text, rubric_version, ground_truth)
        finally:
            client.close()
    return 0


def _judge_one(conn, client, cfg, row, rubric_text, rubric_version, ground_truth) -> None:
    prompt = build_prompt(
        rubric_text,
        ground_truth.get(row["fixture"], "(no ground truth recorded for this fixture)"),
        row["question_text"] or row["question_id"],
        row["answer"] or "",
        json.loads(row["steps_json"] or "[]"),
    )
    try:
        response = client.post(
            cfg.judge_url.rstrip("/") + "/api/generate",
            json={
                "model": cfg.judge_model,
                "prompt": prompt,
                "stream": False,
                # Unlike the agent, the harness owns this call, so it can pin the
                # serving parameters per request rather than in a Modelfile alias.
                "format": "json",
                "options": {"temperature": 0, "top_p": 1, "seed": 42, "num_ctx": 8192},
            },
        )
        response.raise_for_status()
        raw = response.json().get("response", "")
    except httpx.HTTPError as err:
        store.record_judgement(
            conn,
            row["id"],
            judge_model=cfg.judge_model,
            rubric_version=rubric_version,
            error=f"{type(err).__name__}: {err}",
        )
        log.warning("  result %s: judge unreachable: %s", row["id"], err)
        return

    try:
        verdict = parse_verdict(raw)
    except (ValueError, json.JSONDecodeError) as err:
        store.record_judgement(
            conn,
            row["id"],
            judge_model=cfg.judge_model,
            rubric_version=rubric_version,
            raw_json=raw,
            error=f"unparseable: {err}",
        )
        log.warning("  result %s: unparseable verdict (%s)", row["id"], err)
        return

    store.record_judgement(
        conn,
        row["id"],
        judge_model=cfg.judge_model,
        rubric_version=rubric_version,
        raw_json=raw,
        **verdict,
    )
    log.info(
        "  result %s (%s): %s score=%d",
        row["id"],
        row["fixture"],
        verdict["verdict"],
        verdict["score"],
    )
