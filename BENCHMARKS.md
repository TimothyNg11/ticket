# Benchmarks

Each load or chaos run is recorded here with its settings and results: find the bottleneck, fix it, rerun, and keep the before-and-after numbers. Every run ends with the invariant checker (zero seats with two tickets, every sold seat tied to a confirmed order, every confirmed order paid exactly once).

Runs start in Phase 9.

| Date | Scenario | Users | Throughput (req/s) | p50 | p99 | Error rate | Cache hit rate | Invariant violations | Notes |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
