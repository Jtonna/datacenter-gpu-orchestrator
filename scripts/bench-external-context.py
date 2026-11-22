#!/usr/bin/env python3
"""Turn a context-scaling summary.json (and its ladder.json) into an external result set.

The summary maps a model label to {"profile", "nodes", "rows": [{"ctx",
"fill_tokens", "fill_pct", "load_s", "cold_prompt_tps", "cold_ttft_s",
"warm_ttft_s", "gen_tps", "recall": "n/5", "mem_low": {node: MiB},
"verdict", "status", "reason"}]}. The ladder, optional, is a list of
{"model", "ctx", "profile", "need_mib", "measured_need_mib", "result"}: it
gives each step's own profile and the steps that were never sent.

    scripts/bench-external-context.py summary.json [--ladder ladder.json] [--post URL]

Prints the body of POST /api/bench/external, or sends it with --post (the
scheduler's base URL). --replace sends a PUT instead. Python 3 standard
library only.
"""
import argparse
import json
import os
import sys
import urllib.error
import urllib.request

MAX_COLUMNS = 60


def col(key, label, unit="", better=None):
    c = {"key": key, "label": label}
    if unit:
        c["unit"] = unit
    if better is not None:
        c["higher_is_better"] = better
    return c


def num(v):
    return v if isinstance(v, (int, float)) and not isinstance(v, bool) else None


def recall_n(v):
    if isinstance(v, str) and "/" in v:
        try:
            return int(v.split("/")[0])
        except ValueError:
            return None
    return num(v)


def convert(summary, ladder, args):
    steps = {(e.get("model"), e.get("ctx")): e for e in ladder or []}
    main = {
        "title": "Context steps",
        "description": "One row per model and context size: prompt size, load, cold and warm time to first token, generation speed, recall and the lowest available memory of any node.",
        "columns": [col("model", "Model"), col("ctx", "Context", "tokens"), col("prompt_tokens", "Prompt", "tokens"),
                    col("fill_pct", "Fill", "%"), col("load_s", "Load", "s", False),
                    col("cold_prompt_tps", "Cold prompt", "tok/s", True), col("cold_ttft_s", "Cold first token", "s", False),
                    col("warm_ttft_s", "Warm first token", "s", False), col("gen_tps", "Generation", "tok/s", True),
                    col("recall", "Recall", "of 5", True), col("lowest_mib", "Lowest available", "MiB", True),
                    col("lowest_node", "Lowest node"), col("need_mib", "Planned need", "MiB"),
                    col("measured_need_mib", "Measured need", "MiB"), col("verdict", "Verdict"), col("reason", "Reason")],
        "rows": [],
    }
    nodes = []
    mem_rows = []
    seen = set()
    for model in summary:
        m = summary[model] or {}
        for r in m.get("rows") or []:
            ctx = r.get("ctx")
            seen.add((model, ctx))
            lad = steps.get((model, ctx), {})
            low = {k: v for k, v in (r.get("mem_low") or {}).items() if num(v) is not None}
            lowest = min(low, key=low.get) if low else None
            main["rows"].append({
                "model": model, "profile": lad.get("profile") or m.get("profile"), "ctx": num(ctx),
                "prompt_tokens": num(r.get("fill_tokens")), "fill_pct": num(r.get("fill_pct")), "load_s": num(r.get("load_s")),
                "cold_prompt_tps": num(r.get("cold_prompt_tps")), "cold_ttft_s": num(r.get("cold_ttft_s")),
                "warm_ttft_s": num(r.get("warm_ttft_s")), "gen_tps": num(r.get("gen_tps")), "recall": recall_n(r.get("recall")),
                "lowest_mib": low[lowest] if lowest else None, "lowest_node": lowest,
                "need_mib": num(lad.get("need_mib")), "measured_need_mib": num(lad.get("measured_need_mib")),
                "verdict": r.get("verdict") or r.get("status"), "reason": r.get("reason")})
            for n in low:
                if n not in nodes:
                    nodes.append(n)
            mem_rows.append({"model": model, "ctx": num(ctx), "low": low})
    for e in ladder or []:  # steps the summary has no row for: never sent, or failed before measuring
        key = (e.get("model"), e.get("ctx"))
        if key in seen or not e.get("model"):
            continue
        res = str(e.get("result") or "")
        verdict, _, reason = res.partition(":")
        main["rows"].append({"model": e["model"], "profile": e.get("profile"), "ctx": num(e.get("ctx")),
                             "need_mib": num(e.get("need_mib")), "measured_need_mib": num(e.get("measured_need_mib")),
                             "verdict": verdict.strip() or None, "reason": reason.strip() or None})
    main["rows"].sort(key=lambda r: (list(summary).index(r["model"]) if r["model"] in summary else len(summary), r.get("ctx") or 0))
    tables = [main]
    nodes.sort()
    shown = nodes[:MAX_COLUMNS - 2]
    if shown:
        keys = {n: "node_%d" % i for i, n in enumerate(shown)}
        tables.append({
            "title": "Lowest available memory per node",
            "description": "The lowest available memory each node reported during the step." +
                           (" %d more nodes are not shown." % (len(nodes) - len(shown)) if len(nodes) > len(shown) else ""),
            "columns": [col("model", "Model"), col("ctx", "Context", "tokens")] + [col(keys[n], n, "MiB", True) for n in shown],
            "rows": [dict({"model": r["model"], "ctx": r["ctx"]}, **{keys[n]: r["low"][n] for n in shown if n in r["low"]}) for r in mem_rows],
        })
    out = {"id": args.id, "title": args.title, "description": args.description or
           "%d models at growing context sizes: prompt processing, time to first token, generation speed, recall and memory." % len(summary),
           "tables": tables}
    if args.note:
        out["note"] = args.note
    atts = []
    for p in args.attach or []:
        with open(p, encoding="utf-8") as f:
            atts.append({"name": os.path.basename(p), "text": f.read()})
    if atts:
        out["attachments"] = atts
    return out


def send(body, base, replace, actor):
    path = "/api/bench/external" + ("/" + body["id"] if replace else "")
    req = urllib.request.Request(base.rstrip("/") + path, data=json.dumps(body).encode("utf-8"), method="PUT" if replace else "POST",
                                 headers={"Content-Type": "application/json", "X-Actor": actor})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            print("%s %s: %d" % (req.get_method(), path, r.status))
            return 0
    except urllib.error.HTTPError as e:
        print("%s %s: %d %s" % (req.get_method(), path, e.code, e.read().decode("utf-8", "replace")), file=sys.stderr)
        return 1


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("summary", help="the context summary.json")
    ap.add_argument("--ladder", help="the ladder.json beside it (each step's profile, planned need and the steps never sent)")
    ap.add_argument("--id", default="context-scaling", help="the set's id (default %(default)s)")
    ap.add_argument("--title", default="Context scaling")
    ap.add_argument("--description", default="")
    ap.add_argument("--note", default="", help="free text kept with the set")
    ap.add_argument("--attach", action="append", help="a text file to attach (repeat for more)")
    ap.add_argument("--post", metavar="URL", help="send to this scheduler instead of printing")
    ap.add_argument("--replace", action="store_true", help="with --post: PUT, replacing a set of the same id")
    ap.add_argument("--actor", default="bench-external-context", help="the X-Actor of the request")
    args = ap.parse_args()
    with open(args.summary, encoding="utf-8") as f:
        summary = json.load(f)
    ladder = None
    if args.ladder:
        with open(args.ladder, encoding="utf-8") as f:
            ladder = json.load(f)
    body = convert(summary, ladder, args)
    if args.post:
        return send(body, args.post, args.replace, args.actor)
    json.dump(body, sys.stdout, indent=1)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
