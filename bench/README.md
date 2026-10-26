# Benchmarking

Run everything from the repository root.

A [k6](https://k6.io) load test where each virtual user reads like the iOS app:
library -> title -> chapter manifest -> every page with 2–6 s think time -> progress
batch. It passes with p95 < 500 ms, p99 < 1 s and under 1% errors.

```bash
make bench-seed                          # bench/data: 10 titles x 10 chapters x 20 pages (~300 MB)
make bench-serve                         # fresh server on 127.0.0.1:8090, scheduled jobs off
make bench ARGS="-e VUS=1000"            # in another terminal; DURATION defaults to 5m
```

Seed size: `make bench-seed ARGS="-titles 50 -pages 30 -page-kb 300"`. To test a
real server: `k6 run -e BASE_URL=https://… -e USERNAME=… -e PASSWORD=… bench/load.js`.
The score is the most readers that pass a 5-minute hold. Run k6 on a separate
machine when you can, since k6 on the same box takes CPU away from the server.

To approximate a smaller home server, run the server in Docker with CPU and RAM
caps instead of `make bench-serve`. `CPUS` is the target's Geekbench 6
multi-core score divided by one core of the machine running Docker; these values
assume an M1 Max (my machine heh) (one core is about 2,400):

| Machine | Command |
| --- | --- |
| Raspberry Pi 4 | `make bench-serve-docker CPUS=0.3 MEM=2g` |
| Intel N100 mini PC | `make bench-serve-docker CPUS=1.3 MEM=8g` |

This caps CPU and memory but not disk speed. To test a real drive, seed the CBZs
onto it with `make bench-seed BENCH_CBZ="/Volumes/<drive>/kaodoku-bench" ARGS="-titles 100"`
(about 3 GB, so most reads miss the cache), run `sudo purge` to empty the page cache,
then start the server. With `bench-serve-docker`, pass the same `BENCH_CBZ`.

Inspired by this neat thing [here](https://github.com/arjaythedev/twelve-dollar-server-challenge/tree/main).
