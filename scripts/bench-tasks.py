#!/usr/bin/env python3
"""Move coding tasks, answers and scores between Potluck and a scoring kit.

Potluck's tasks phase sends a fixed task set to a model and stores every
answer; it never runs or scores them. This script links it to a kit that
does (the coding kit: run the tests in a sandbox, a rubric for the rest):

  bench-tasks.py taskset KIT [--id coding --version 1]
      Print a task set (format potluck-tasks/1) built from a kit folder:
      KIT/system_prompt.txt and KIT/tasks/<id>/{meta.json,prompt.md}
      (meta: title, max_tokens, time_limit_s). --post URL stores it
      (POST /api/bench/task-sets).

  bench-tasks.py pull URL RUN --out DIR [--step N] [--label NAME]
      Write the run's answers in the kit's layout: DIR/<label>/<task>.json
      (answer, reasoning, finish_reason, usage, error, and a "potluck" block
      naming the run, step, phase and result id) and <task>.answer.md. The
      label is <profile>-step<N> unless --label is given. Then score the
      folder with the kit (score.py --out DIR).

  bench-tasks.py scores DIR --scorer NAME [--rule strict|lenient] [--max-points P]
      Print a scores body from the kit's DIR/<label>/<task>.score.json files
      (tests_passed of tests_total; --max-points P scales them to P points a
      task). --post URL sends it (PUT /api/bench/runs/<run>/scores/<scorer>)
      for every run found in DIR.

  bench-tasks.py post URL RUN SCORER FILE
      Send a ready scores body (for example a rubric scorer's output):
      {"scorer_version": "...", "scores": [{"step": 0, "task": "t1",
      "points": 12, "max_points": 17, "notes": "..."}]}

A scorer's post replaces its earlier scores on the run, so posting again is
safe. Python 3 standard library only.
"""
import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

FORMAT = "potluck-tasks/1"


def read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def write_json(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(data, f, indent=2, ensure_ascii=False)
        f.write("\n")


def request(base, method, path, body=None, actor="bench-tasks"):
    data = None if body is None else json.dumps(body).encode("utf-8")
    req = urllib.request.Request(base.rstrip("/") + path, data=data, method=method,
                                 headers={"Content-Type": "application/json", "X-Actor": actor})
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            return r.status, json.loads(r.read().decode("utf-8") or "null")
    except urllib.error.HTTPError as e:
        raise SystemExit("%s %s: %d %s" % (method, path, e.code, e.read().decode("utf-8", "replace")))


# ---------------------------------------------------------------- taskset


def taskset(args):
    kit = args.kit
    tasks_dir = os.path.join(kit, "tasks")
    if not os.path.isdir(tasks_dir):
        raise SystemExit("no tasks folder in %s" % kit)
    system_path = os.path.join(kit, "system_prompt.txt")
    out = {"format": FORMAT, "id": args.id, "version": args.version, "title": args.title or args.id,
           "temperature": 0, "seed": args.seed, "tasks": []}
    if args.description:
        out["description"] = args.description
    if os.path.isfile(system_path):
        out["system"] = read(system_path).strip()
    for name in sorted(os.listdir(tasks_dir)):
        folder = os.path.join(tasks_dir, name)
        meta_path, prompt_path = os.path.join(folder, "meta.json"), os.path.join(folder, "prompt.md")
        if not (os.path.isfile(meta_path) and os.path.isfile(prompt_path)):
            continue
        meta = json.loads(read(meta_path))
        t = {"id": meta.get("id") or name, "prompt": read(prompt_path)}
        if meta.get("title"):
            t["title"] = meta["title"]
        if meta.get("max_tokens"):
            t["max_tokens"] = int(meta["max_tokens"])
        if meta.get("time_limit_s"):
            t["timeout_s"] = int(meta["time_limit_s"])
        out["tasks"].append(t)
    if not out["tasks"]:
        raise SystemExit("no task in %s (each needs meta.json and prompt.md)" % tasks_dir)
    if args.post:
        status, got = request(args.post, "POST", "/api/bench/task-sets", out, args.actor)
        print("POST /api/bench/task-sets: %d, %s version %s, sha256 %s%s" % (
            status, got.get("id"), got.get("version"), got.get("sha256"), "" if got.get("created") else " (already stored)"))
        return 0
    json.dump(out, sys.stdout, indent=1, ensure_ascii=False)
    sys.stdout.write("\n")
    return 0


# ---------------------------------------------------------------- pull


def pull(args):
    q = {"answers": "1"}
    if args.step is not None:
        q["step"] = str(args.step)
    path = "/api/bench/runs/%s/tasks?%s" % (urllib.parse.quote(args.run, safe=""), urllib.parse.urlencode(q))
    _, doc = request(args.url, "GET", path, actor=args.actor)
    tasks = doc.get("tasks") or []
    if not tasks:
        print("run %s has no task results%s" % (args.run, "" if args.step is None else " in step %d" % args.step))
        return 1
    phases = {}
    for t in tasks:
        phases.setdefault(t["step"], set()).add(t["phase"])
    written = 0
    for t in tasks:
        label = args.label or "%s-step%d" % (t.get("profile") or "profile", t["step"])
        if len(phases[t["step"]]) > 1:
            label += "-phase%d" % t["phase"]
        usage = {"prompt_tokens": t.get("prompt_tokens", 0), "completion_tokens": t.get("completion_tokens", 0)}
        record = {
            "task": t["task_id"],
            "label": label,
            "profile": t.get("profile"),
            "answer": t.get("answer") or "",
            "reasoning": t.get("reasoning") or "",
            "finish_reason": t.get("finish_reason"),
            "usage": usage,
            "ttft_s": None if t.get("ttft_ms") is None else round(t["ttft_ms"] / 1000.0, 3),
            "wall_s": round((t.get("wall_ms") or 0) / 1000.0, 3),
            "timings": {"predicted_per_second": t.get("decode_tps")},
            "http_status": t.get("status") or None,
            "error": t.get("error") or None,
            "potluck": {"run_id": args.run, "step": t["step"], "phase": t["phase"], "result_id": t["id"], "task_id": t["task_id"],
                        "state": t.get("state"), "benchmark_id": t.get("benchmark_id"),
                        "set_id": t.get("set_id"), "set_version": t.get("set_version"), "set_sha256": t.get("set_sha256")},
        }
        if t.get("state") == "not_sent":
            continue
        folder = os.path.join(args.out, label)
        write_json(os.path.join(folder, t["task_id"] + ".json"), record)
        with open(os.path.join(folder, t["task_id"] + ".answer.md"), "w", encoding="utf-8", newline="\n") as f:
            f.write(record["answer"])
        written += 1
    print("wrote %d answers of run %s under %s" % (written, args.run, args.out))
    return 0


# ---------------------------------------------------------------- scores


def collect_scores(args):
    """{run_id: [score]} from DIR/<label>/<task>.score.json next to the pulled <task>.json."""
    by_run = {}
    if not os.path.isdir(args.dir):
        raise SystemExit("no such folder: %s" % args.dir)
    for label in sorted(os.listdir(args.dir)):
        folder = os.path.join(args.dir, label)
        if label.startswith("_") or not os.path.isdir(folder):
            continue
        for name in sorted(os.listdir(folder)):
            if not name.endswith(".score.json"):
                continue
            task = name[: -len(".score.json")]
            answer_path = os.path.join(folder, task + ".json")
            if not os.path.isfile(answer_path):
                print("skipped %s/%s: no %s.json next to it" % (label, name, task), file=sys.stderr)
                continue
            pot = json.loads(read(answer_path)).get("potluck")
            if not pot or not pot.get("result_id"):
                print("skipped %s/%s: its answer was not pulled from Potluck" % (label, name), file=sys.stderr)
                continue
            sc = json.loads(read(os.path.join(folder, name)))
            part = sc.get(args.rule) or sc
            passed, total = part.get("tests_passed"), part.get("tests_total")
            if not isinstance(passed, (int, float)) or not isinstance(total, (int, float)) or total <= 0:
                print("skipped %s/%s: no tests_passed/tests_total" % (label, name), file=sys.stderr)
                continue
            points, max_points = float(passed), float(total)
            if args.max_points:
                points, max_points = round(args.max_points * passed / total, 3), float(args.max_points)
            s = {"result": pot["result_id"], "points": points, "max_points": max_points}
            failed = part.get("failed_tests") or []
            if failed:
                s["failed_tests"] = [str(x)[:300] for x in failed][:500]
            notes = [n for n in [part.get("reason")] + list(part.get("notes") or []) if n]
            if part.get("timeout"):
                notes.insert(0, "tests timed out")
            if notes:
                s["notes"] = "; ".join(str(n) for n in notes)[:4000]
            by_run.setdefault(pot["run_id"], []).append(s)
    return by_run


def scores(args):
    by_run = collect_scores(args)
    if not by_run:
        print("no scored answers under %s" % args.dir, file=sys.stderr)
        return 1
    version = args.scorer_version or ("score.py %s" % args.rule)
    docs = {run: {"scorer_version": version, "scores": s} for run, s in by_run.items()}
    if args.post:
        for run, doc in sorted(docs.items()):
            _, got = request(args.post, "PUT", "/api/bench/runs/%s/scores/%s" % (urllib.parse.quote(run, safe=""), urllib.parse.quote(args.scorer, safe="")), doc, args.actor)
            tot = got.get("total") or {}
            print("run %s, scorer %s: %s of %s points over %s of %s tasks" % (run, args.scorer, tot.get("points"), tot.get("max_points"),
                                                                             tot.get("tasks_scored"), tot.get("tasks")))
        return 0
    out = docs[next(iter(docs))] if len(docs) == 1 else {"runs": docs}
    json.dump(out, sys.stdout, indent=1, ensure_ascii=False)
    sys.stdout.write("\n")
    return 0


def post(args):
    doc = json.loads(read(args.file))
    _, got = request(args.url, "PUT", "/api/bench/runs/%s/scores/%s" % (urllib.parse.quote(args.run, safe=""), urllib.parse.quote(args.scorer, safe="")), doc, args.actor)
    tot = got.get("total") or {}
    print("run %s, scorer %s: %s of %s points over %s of %s tasks" % (args.run, args.scorer, tot.get("points"), tot.get("max_points"),
                                                                     tot.get("tasks_scored"), tot.get("tasks")))
    return 0


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--actor", default="bench-tasks", help="the X-Actor of requests")
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("taskset", help="build a task set from a kit folder")
    p.add_argument("kit")
    p.add_argument("--id", default="coding")
    p.add_argument("--version", type=int, default=1)
    p.add_argument("--title", default="")
    p.add_argument("--description", default="")
    p.add_argument("--seed", type=int, default=1)
    p.add_argument("--post", metavar="URL")
    p.set_defaults(fn=taskset)
    p = sub.add_parser("pull", help="write a run's answers in the kit's layout")
    p.add_argument("url")
    p.add_argument("run")
    p.add_argument("--out", required=True)
    p.add_argument("--step", type=int)
    p.add_argument("--label")
    p.set_defaults(fn=pull)
    p = sub.add_parser("scores", help="turn the kit's score files into scores")
    p.add_argument("dir")
    p.add_argument("--scorer", required=True)
    p.add_argument("--scorer-version", default="")
    p.add_argument("--rule", choices=["strict", "lenient"], default="strict")
    p.add_argument("--max-points", type=float, default=0, help="scale each task to this many points")
    p.add_argument("--post", metavar="URL")
    p.set_defaults(fn=scores)
    p = sub.add_parser("post", help="send a ready scores body")
    p.add_argument("url")
    p.add_argument("run")
    p.add_argument("scorer")
    p.add_argument("file")
    p.set_defaults(fn=post)
    args = ap.parse_args()
    return args.fn(args)


if __name__ == "__main__":
    sys.exit(main())
