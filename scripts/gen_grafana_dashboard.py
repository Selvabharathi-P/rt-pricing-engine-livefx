"""Generates deploy/grafana/dashboards/pricing-engine.json.

Usage: python scripts/gen_grafana_dashboard.py .   (from the repo root)
"""
import json
import sys
from pathlib import Path

DS = {"type": "prometheus", "uid": "prom"}
panels = []
pid = 0
y = 0


def nid():
    global pid
    pid += 1
    return pid


def row(title):
    global y
    panels.append({"type": "row", "title": title, "id": nid(), "collapsed": False,
                   "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "panels": []})
    y += 1


def target(expr, legend, ref):
    return {"datasource": DS, "expr": expr, "legendFormat": legend, "refId": ref, "range": True}


def ts(title, x, w, h, queries, unit="short", desc="", stack=False, min0=True, maxv=None, novalue="No data"):
    p = {"type": "timeseries", "title": title, "id": nid(), "datasource": DS, "description": desc,
         "gridPos": {"h": h, "w": w, "x": x, "y": y},
         "targets": [target(e, l, chr(65 + i)) for i, (e, l) in enumerate(queries)],
         "fieldConfig": {"defaults": {"unit": unit, "min": 0 if min0 else None, "noValue": novalue,
                                      "custom": {"lineWidth": 2, "fillOpacity": 12, "showPoints": "never",
                                                 "stacking": {"mode": "normal" if stack else "none"}}},
                         "overrides": []},
         "options": {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi"}}}
    if not min0:
        del p["fieldConfig"]["defaults"]["min"]
    if maxv is not None:
        p["fieldConfig"]["defaults"]["max"] = maxv
    panels.append(p)


def stat(title, x, w, expr, unit="short", desc="", mappings=None, thresholds=None, decimals=None, novalue="–"):
    d = {"unit": unit, "mappings": mappings or [], "noValue": novalue,
         "thresholds": {"mode": "absolute", "steps": thresholds or [{"color": "blue", "value": None}]}}
    if decimals is not None:
        d["decimals"] = decimals
    panels.append({"type": "stat", "title": title, "id": nid(), "datasource": DS, "description": desc,
                   "gridPos": {"h": 4, "w": w, "x": x, "y": y},
                   "targets": [target(expr, "", "A")],
                   "fieldConfig": {"defaults": d, "overrides": []},
                   "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                               "colorMode": "background", "graphMode": "area", "textMode": "value",
                               "justifyMode": "center", "orientation": "auto"}})


q = lambda h, p: f"histogram_quantile({p}, sum(rate({h}_bucket[1m])) by (le))"

# ---------------------------------------------------------------- overview
row("Overview")
stat("Upstream feed", 0, 4, "pe_upstream_connected", desc="LiveFXHub session: 1 = connected.",
     mappings=[{"type": "value", "options": {"0": {"text": "DOWN", "color": "red"}, "1": {"text": "CONNECTED", "color": "green"}}}],
     thresholds=[{"color": "red", "value": None}, {"color": "green", "value": 1}])
stat("Ticks / s", 4, 4, "sum(rate(pe_ticks_total[1m]))", unit="reqps", decimals=1,
     thresholds=[{"color": "orange", "value": None}, {"color": "green", "value": 1}])
stat("WebSocket clients", 8, 4, "pe_ws_clients", thresholds=[{"color": "blue", "value": None}])
stat("Process latency p99", 12, 4, f"{q('pe_process_latency_seconds', 0.99)}", unit="s",
     desc="Upstream receive → handed to WebSocket hub.", novalue="no ticks",
     thresholds=[{"color": "green", "value": None}, {"color": "orange", "value": 0.025}, {"color": "red", "value": 0.05}])
stat("HTTP 5xx / 5m", 16, 4, "increase(pe_http_responses_5xx_total[5m])", decimals=0,
     thresholds=[{"color": "green", "value": None}, {"color": "red", "value": 1}])
stat("Redis flush errors / 5m", 20, 4, "increase(pe_store_flush_errors_total[5m])", decimals=0,
     thresholds=[{"color": "green", "value": None}, {"color": "red", "value": 1}])
y += 4

# ---------------------------------------------------------------- pipeline
row("Price pipeline")
ts("Upstream frames & accepted ticks", 0, 12, 8, [
    ("sum(rate(pe_upstream_frames_total[1m]))", "upstream frames/s"),
    ("sum(rate(pe_ticks_total[1m]))", "ticks accepted/s")], unit="reqps")
ts("Dropped / corrected ticks", 12, 12, 8, [
    ("sum(rate(pe_ticks_duplicate_total[1m]))", "duplicates/s"),
    ("sum(rate(pe_ticks_out_of_order_total[1m]))", "out of order/s"),
    ("sum(rate(pe_ticks_rejected_total[1m]))", "rejected/s"),
    ("sum(rate(pe_upstream_parse_errors_total[1m])) + sum(rate(pe_upstream_unrecognised_frames_total[1m]))", "unparsed frames/s")],
   unit="reqps", desc="Non-zero 'unparsed' means the upstream format changed.")
y += 8
ts("Upstream connection", 0, 12, 6, [("pe_upstream_connected", "connected (1/0)")], maxv=1,
   desc="Drops to 0 when LiveFXHub is unavailable or the session is held by another client.")
ts("Upstream reconnect attempts", 12, 12, 6, [("increase(pe_upstream_reconnects_total[5m])", "reconnects / 5m")])
y += 6

# ---------------------------------------------------------------- latency
row("Latency")
ts("Engine processing (receive → hub)", 0, 12, 8, [
    (q("pe_process_latency_seconds", 0.5), "p50"),
    (q("pe_process_latency_seconds", 0.95), "p95"),
    (q("pe_process_latency_seconds", 0.99), "p99")], unit="s", novalue="No ticks (feed down?)")
ts("End to end (receive → client socket)", 12, 12, 8, [
    (q("pe_ws_send_latency_seconds", 0.5), "p50"),
    (q("pe_ws_send_latency_seconds", 0.95), "p95"),
    (q("pe_ws_send_latency_seconds", 0.99), "p99")], unit="s", desc="Target: ≤ 50 ms.", novalue="No ticks sent")
y += 8

# ---------------------------------------------------------------- fan-out
row("WebSocket fan-out")
ts("Clients & frames", 0, 8, 8, [
    ("pe_ws_clients", "clients"),
    ("sum(rate(pe_ws_frames_total[1m]))", "frames sent/s")])
ts("Frame size (pre-compression)", 8, 8, 8, [
    (q("pe_ws_frame_bytes", 0.5), "p50"),
    (q("pe_ws_frame_bytes", 0.99), "p99"),
    ("vector(10240)", "10 KB limit")], unit="bytes", novalue="No frames sent")
ts("Slow clients", 16, 8, 8, [
    ("sum(rate(pe_ws_conflated_total[1m]))", "quotes conflated/s"),
    ("increase(pe_ws_slow_disconnects_total[5m])", "slow disconnects / 5m")],
   desc="Conflated = a newer quote replaced one a slow client hadn't received yet.")
y += 8

# ---------------------------------------------------------------- persistence
row("Redis persistence")
ts("Flush duration", 0, 12, 7, [
    (q("pe_store_flush_seconds", 0.5), "p50"),
    (q("pe_store_flush_seconds", 0.99), "p99")], unit="s", desc="One flush per second of changed buckets.", novalue="Nothing to flush (no ticks)")
ts("Flushes & errors", 12, 12, 7, [
    ("sum(rate(pe_store_flush_seconds_count[1m])) * 60", "successful flushes / min"),
    ("increase(pe_store_flush_errors_total[5m])", "errors / 5m")])
y += 7

# ---------------------------------------------------------------- http
row("HTTP")
ts("Requests by class", 0, 12, 7, [
    ("sum(rate(pe_http_requests_total[1m])) - sum(rate(pe_http_responses_4xx_total[1m])) - sum(rate(pe_http_responses_5xx_total[1m]))", "2xx/3xx"),
    ("sum(rate(pe_http_responses_4xx_total[1m]))", "4xx"),
    ("sum(rate(pe_http_responses_5xx_total[1m]))", "5xx")], unit="reqps", stack=True)
ts("Request latency", 12, 12, 7, [
    (q("pe_http_request_seconds", 0.5), "p50"),
    (q("pe_http_request_seconds", 0.95), "p95"),
    (q("pe_http_request_seconds", 0.99), "p99")], unit="s", desc="Excludes WebSocket sessions.")
y += 7

# ---------------------------------------------------------------- runtime
row("Runtime")
ts("Goroutines", 0, 8, 6, [("pe_go_goroutines", "goroutines")])
ts("Heap in use", 8, 8, 6, [("pe_go_heap_alloc_bytes", "heap")], unit="bytes")
ts("Uptime", 16, 8, 6, [("pe_uptime_seconds", "uptime")], unit="s", desc="Drops to 0 on restart.")
y += 6

dash = {
    "uid": "pricing-engine", "title": "Pricing Engine", "tags": ["pricing-engine"],
    "timezone": "browser", "editable": False, "graphTooltip": 1, "schemaVersion": 39, "version": 1,
    "refresh": "5s", "time": {"from": "now-30m", "to": "now"},
    "timepicker": {"refresh_intervals": ["5s", "10s", "30s", "1m", "5m"]},
    "annotations": {"list": []}, "templating": {"list": []}, "links": [], "panels": panels,
}
out = Path(sys.argv[1]) / "deploy/grafana/dashboards/pricing-engine.json"
out.parent.mkdir(parents=True, exist_ok=True)
out.write_text(json.dumps(dash, indent=2) + "\n", encoding="utf-8")
print(f"wrote {out} with {sum(1 for p in panels if p['type'] != 'row')} panels")
