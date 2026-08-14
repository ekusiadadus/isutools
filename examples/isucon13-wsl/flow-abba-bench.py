#!/usr/bin/env python3
"""Small fixed-workload probe for the flow-label ABBA check."""

import concurrent.futures
import http.client
import json
import math
import os
import ssl
import time

REQUESTS = int(os.environ.get("FLOW_BENCH_REQUESTS", "500"))
CONCURRENCY = int(os.environ.get("FLOW_BENCH_CONCURRENCY", "10"))
PATH = os.environ.get("FLOW_BENCH_PATH", "/api/tag")
COOKIE = "SESSIONID=isutools-flow-abba"


def worker(count: int) -> tuple[list[float], int]:
    context = ssl._create_unverified_context()
    conn = http.client.HTTPSConnection("127.0.0.1", 443, context=context, timeout=5)
    latencies: list[float] = []
    failed = 0
    try:
        for _ in range(count):
            started = time.perf_counter()
            try:
                conn.request(
                    "GET",
                    PATH,
                    headers={"Host": "pipe.u.isucon.local", "Cookie": COOKIE},
                )
                response = conn.getresponse()
                response.read()
                if response.status != 200:
                    failed += 1
            except Exception:
                failed += 1
                conn.close()
                conn = http.client.HTTPSConnection(
                    "127.0.0.1", 443, context=context, timeout=5
                )
            latencies.append((time.perf_counter() - started) * 1000)
    finally:
        conn.close()
    return latencies, failed


def main() -> None:
    if REQUESTS < 1 or CONCURRENCY < 1:
        raise SystemExit("FLOW_BENCH_REQUESTS and CONCURRENCY must be positive")
    workers = min(REQUESTS, CONCURRENCY)
    counts = [REQUESTS // workers] * workers
    for index in range(REQUESTS % workers):
        counts[index] += 1
    started = time.perf_counter()
    latencies: list[float] = []
    failed = 0
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as executor:
        for values, errors in executor.map(worker, counts):
            latencies.extend(values)
            failed += errors
    elapsed = time.perf_counter() - started
    latencies.sort()
    p95 = latencies[max(0, math.ceil(len(latencies) * 0.95) - 1)]
    print(
        json.dumps(
            {
                "score": round((REQUESTS - failed) / elapsed, 3),
                "p95_ms": round(p95, 3),
                "error_rate": round(failed / REQUESTS, 6),
            },
            separators=(",", ":"),
        )
    )


if __name__ == "__main__":
    main()
