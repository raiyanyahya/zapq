#!/usr/bin/env python
"""
Usage: python analyze_results.py results.csv
Creates: summary.txt  histogram_enqueue.png  histogram_dequeue.png
"""
import sys, pandas as pd, matplotlib.pyplot as plt, numpy as np, textwrap, json, os

csv = pd.read_csv(sys.argv[1])
for kind in ("enqueue", "dequeue"):
    subset = csv[csv.kind == kind]["latency_ns"] / 1e6  # → ms
    stats = {
        "count": int(subset.count()),
        "avg_ms": subset.mean(),
        "p50_ms": subset.quantile(0.50),
        "p95_ms": subset.quantile(0.95),
        "p99_ms": subset.quantile(0.99),
        "max_ms": subset.max(),
    }
    # write histogram
    fig, ax = plt.subplots()
    ax.hist(subset, bins=np.logspace(-3, 3, 50))
    ax.set_xscale('log')
    ax.set_xlabel("latency (ms, log scale)")
    ax.set_ylabel("frequency")
    ax.set_title(f"{kind} latency histogram")
    fig.savefig(f"histogram_{kind}.png")
    plt.close(fig)

    # append to summary
    with open("summary.txt", "a") as f:
        f.write(kind.upper()+"\n")
        f.write(json.dumps(stats, indent=2)+"\n\n")

print("Report files generated:", os.listdir("."))
