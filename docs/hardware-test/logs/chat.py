#!/usr/bin/env python3
# usage: chat.py MODEL MAX_TOKENS [PROMPT] [TIMEOUT_S]  -> prints one JSON line
import json, sys, time, urllib.request, urllib.error
model = sys.argv[1]; maxt = int(sys.argv[2])
prompt = sys.argv[3] if len(sys.argv) > 3 else "Say hello in one short sentence."
tmo = float(sys.argv[4]) if len(sys.argv) > 4 else 1800
body = json.dumps({"model": model, "stream": True, "max_tokens": maxt,
                   "messages": [{"role": "user", "content": prompt}]}).encode()
req = urllib.request.Request("http://127.0.0.1:3199/v1/chat/completions", data=body,
                             headers={"Content-Type": "application/json"})
t0 = time.time(); out = {"model": model, "t_sent": time.strftime("%H:%M:%S", time.localtime(t0))}
ttft = None; text = []; timings = None; usage = None; nchunks = 0
try:
    with urllib.request.urlopen(req, timeout=tmo) as r:
        out["status"] = r.status
        for raw in r:
            line = raw.decode("utf-8", "replace").strip()
            if not line.startswith("data:"): continue
            d = line[5:].strip()
            if d == "[DONE]": break
            try: j = json.loads(d)
            except Exception: continue
            nchunks += 1
            if "timings" in j: timings = j["timings"]
            if j.get("usage"): usage = j["usage"]
            if "error" in j: out["stream_error"] = j["error"]
            for c in j.get("choices", []):
                delta = c.get("delta", {})
                piece = (delta.get("content") or "") + (delta.get("reasoning_content") or "")
                if piece:
                    if ttft is None: ttft = time.time() - t0
                    text.append(piece)
except urllib.error.HTTPError as e:
    out["status"] = e.code; out["body"] = e.read().decode("utf-8", "replace")[-1500:]
    out["retry_after"] = e.headers.get("Retry-After")
except Exception as e:
    out["status"] = "exc"; out["error"] = repr(e)
out["total_s"] = round(time.time() - t0, 2)
out["ttft_s"] = round(ttft, 2) if ttft is not None else None
out["chunks"] = nchunks
if timings:
    out["predicted_n"] = timings.get("predicted_n"); out["gen_tps"] = round(timings.get("predicted_per_second") or 0, 2)
    out["prompt_n"] = timings.get("prompt_n"); out["prompt_tps"] = round(timings.get("prompt_per_second") or 0, 2)
out["text"] = "".join(text)[:120]
print(json.dumps(out), flush=True)
