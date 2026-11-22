#!/usr/bin/env python3
"""Turn a coding-quality summary.json into an external result set.

The summary has {"tasks": ["t1", ...], "models": [{"label", "profile",
"load_s", "strict", "lenient", "total", "limit_no_code", "tasks": {"t1":
{"strict", "lenient", "total", "completion_tokens", "end", "tok_s",
"seconds", "strict_reason", "lenient_reason", "error"}}, "rubric_<task>":
{<score>: n, ..., "note": "..."}}]}.

    scripts/bench-external-coding.py summary.json [--attach t1.md ...] [--post URL]

Prints the body of POST /api/bench/external, or sends it with --post (the
scheduler's base URL, for example http://127.0.0.1:8080). --replace sends a
PUT to replace a set of the same id. Python 3 standard library only.
"""
import argparse
import json
import os
import sys
import urllib.error
import urllib.request

def col(key, label, unit="", better=None):
    c = {"key": key, "label": label}
    if unit:
        c["unit"] = unit
    if better is not None:
        c["higher_is_better"] = better
    return c


def num(v):
    return v if isinstance(v, (int, float)) and not isinstance(v, bool) else None


def text(v):
    return None if v is None else str(v)


def convert(doc, args):
    tasks = doc.get("tasks") or []
    models = doc.get("models") or []
    totals = {
        "title": "Totals",
        "description": "Tests passed over all tasks, per model.",
        "columns": [col("model", "Model"), col("profile", "Profile"), col("strict", "Strict", "tests", True),
                    col("lenient", "Lenient", "tests", True), col("total", "Total", "tests"),
                    col("strict_pct", "Strict", "%", True), col("load_s", "Load", "s", False),
                    col("limit_no_code", "Cut off without code", "tasks", False)],
        "rows": [],
    }
    per_task = {
        "title": "Tasks",
        "description": "One row per model and task: tests passed (strict and lenient), the answer's length and speed, and how it ended.",
        "columns": [col("model", "Model"), col("task", "Task"), col("strict", "Strict", "tests", True),
                    col("lenient", "Lenient", "tests", True), col("total", "Total", "tests"),
                    col("completion_tokens", "Completion", "tokens"), col("end", "Ended"),
                    col("tok_s", "Generation", "tok/s", True), col("seconds", "Time", "s", False),
                    col("reason", "Why not scored"), col("error", "Error")],
        "rows": [],
    }
    rubrics = {}  # task -> table
    for m in models:
        label, profile = m.get("label") or m.get("profile") or "?", m.get("profile")
        strict, total = num(m.get("strict")), num(m.get("total"))
        totals["rows"].append({"model": label, "profile": profile, "strict": strict, "lenient": num(m.get("lenient")),
                               "total": total, "strict_pct": round(100.0 * strict / total, 1) if strict is not None and total else None,
                               "load_s": num(m.get("load_s")), "limit_no_code": num(m.get("limit_no_code"))})
        mt = m.get("tasks") or {}
        for t in tasks or sorted(mt):
            r = mt.get(t)
            if r is None:
                continue
            reason = r.get("strict_reason") or r.get("lenient_reason")
            per_task["rows"].append({"model": label, "profile": profile, "task": t, "strict": num(r.get("strict")),
                                     "lenient": num(r.get("lenient")), "total": num(r.get("total")),
                                     "completion_tokens": num(r.get("completion_tokens")), "end": text(r.get("end")),
                                     "tok_s": num(r.get("tok_s")), "seconds": num(r.get("seconds")),
                                     "reason": text(reason), "error": text(r.get("error"))})
        for k, v in m.items():
            if not k.startswith("rubric_") or not isinstance(v, dict):
                continue
            task = k[len("rubric_"):]
            tb = rubrics.setdefault(task, {"title": "Rubric scores, " + task, "description": "Scores given to the answers of task " + task + ".",
                                           "columns": [col("model", "Model")], "rows": []})
            row = {"model": label, "profile": profile}
            for sk, sv in v.items():
                key = "note" if sk == "note" else sk
                if not any(c["key"] == key for c in tb["columns"]):
                    tb["columns"].append(col(key, "Note") if key == "note" else col(key, sk.replace("_", " ").capitalize(), "", True))
                row[key] = text(sv) if key == "note" else num(sv)
            tb["rows"].append(row)
    for tb in rubrics.values():  # the note last
        tb["columns"].sort(key=lambda c: c["key"] == "note")
    out = {"id": args.id, "title": args.title, "description": args.description or
           "%d models, %d tasks: tests passed per task, answer length, speed and how the answer ended." % (len(models), len(tasks)),
           "tables": [totals, per_task] + [rubrics[k] for k in sorted(rubrics)]}
    if args.note:
        out["note"] = args.note
    atts = attachments(args.attach)
    if atts:
        out["attachments"] = atts
    return out


def attachments(paths):
    out = []
    for p in paths or []:
        with open(p, encoding="utf-8") as f:
            out.append({"name": os.path.basename(p), "text": f.read()})
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
    ap.add_argument("summary", help="the coding summary.json")
    ap.add_argument("--id", default="coding-quality", help="the set's id (default %(default)s)")
    ap.add_argument("--title", default="Coding quality")
    ap.add_argument("--description", default="")
    ap.add_argument("--note", default="", help="free text kept with the set")
    ap.add_argument("--attach", action="append", help="a text file to attach (repeat for more), such as a task statement")
    ap.add_argument("--post", metavar="URL", help="send to this scheduler instead of printing")
    ap.add_argument("--replace", action="store_true", help="with --post: PUT, replacing a set of the same id")
    ap.add_argument("--actor", default="bench-external-coding", help="the X-Actor of the request")
    args = ap.parse_args()
    with open(args.summary, encoding="utf-8") as f:
        body = convert(json.load(f), args)
    if args.post:
        return send(body, args.post, args.replace, args.actor)
    json.dump(body, sys.stdout, indent=1)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
