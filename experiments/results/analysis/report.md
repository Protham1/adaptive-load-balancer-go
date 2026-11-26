# Experiment report (Phases 14-17)

Generated 2026-11-26 11:56 UTC from `experiments/results/`.

* Runs: **180** complete, **180** valid, 0 failed, 0 excluded (generator not healthy). Commits recorded in run metadata: e13c34256f; runs from a dirty tree: 0.
* Possible host interference (system CPU > 90% at some second): 6 run(s) - E2_lc_r0, E8_ai_r2, E8_swrr_r2, E9_ai_r0_i10, E9_ai_r2_i30, E9_swrr_r2.
* Window 4 s; warm-up 8 s excluded from every metric; seeds base 1000 + repetition (identical arrival schedule for every policy of a repetition).

**Reading rules.** Cells are `mean ± sd [95 % bootstrap CI of the mean]` over repetitions. Comparisons are paired by repetition. A result is called *better*/*worse* only if n ≥ 5 pairs, the paired 95 % bootstrap CI excludes 0 and the Holm-adjusted exact sign-flip p < 0.05; otherwise it is *inconclusive* (or *insufficient_n* when n < 5, where p < 0.05 is unattainable by construction).

## A1: Affinity workload (Consistent Hash)

*Question:* Is session affinity preserved, and at what cost?  
*Scenario:* `a1_affinity.json`, pool `balanced`, 36 s; runs per condition: 3.

| condition | p95 latency (ms) | p99 latency (ms) | error rate | OK throughput (rps) | imbalance CV | stickiness (1 = full affinity) |
|---|---|---|---|---|---|---|
| **rr** | 22.9 ± 0.6 [22.5, 23.6] | 23.6 ± 0.0 [23.6, 23.6] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 399.6 ± 4.2 [396.5, 404.4] | 0.000 ± 0.000 [0.000, 0.000] | 0.394 ± 0.002 [0.392, 0.395] |
| **lc** | 22.9 ± 0.6 [22.5, 23.6] | 23.6 ± 0.0 [23.6, 23.6] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 399.7 ± 4.2 [396.5, 404.4] | 0.003 ± 0.002 [0.002, 0.004] | 0.392 ± 0.002 [0.390, 0.393] |
| **swrr** | 22.9 ± 0.6 [22.5, 23.6] | 24.4 ± 1.4 [23.6, 26.0] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 399.6 ± 4.2 [396.5, 404.4] | 0.000 ± 0.000 [0.000, 0.000] | 0.393 ± 0.001 [0.391, 0.394] |
| **ch** | 22.5 ± 0.0 [22.5, 22.5] | 23.2 ± 0.6 [22.5, 23.6] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 399.6 ± 4.2 [396.5, 404.4] | 0.064 ± 0.022 [0.040, 0.082] | 1.000 ± 0.000 [1.000, 1.000] |
| **rule** | 22.5 ± 0.0 [22.5, 22.5] | 22.9 ± 0.6 [22.5, 23.6] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 399.7 ± 4.2 [396.5, 404.4] | 0.000 ± 0.000 [0.000, 0.000] | 0.392 ± 0.001 [0.391, 0.393] |
| **ai** | 23.6 ± 1.2 [22.5, 24.8] | 29.5 ± 5.2 [23.6, 33.2] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 399.7 ± 4.2 [396.5, 404.4] | 0.013 ± 0.023 [0.000, 0.040] | 0.549 ± 0.269 [0.393, 0.860] |

Paired comparisons for p95 and error rate (candidate − reference; negative is better). All metrics are in `comparisons.csv`. With n = 3 pairs no verdict can be anything but `insufficient_n` (smallest attainable p = 0.250); the differences are descriptive.

| metric | candidate vs reference | mean diff [95% CI] | p (Holm) | verdict |
|---|---|---|---|---|
| p95 latency (ms) | rr vs swrr | 0.0 [0.0, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | lc vs swrr | 0.0 [0.0, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ch vs swrr | -0.4 [-1.1, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule vs swrr | -0.4 [-1.1, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai vs swrr | 0.8 [0.0, 1.2] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rr vs lc | 0.0 [0.0, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ch vs lc | -0.4 [-1.1, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule vs lc | -0.4 [-1.1, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai vs lc | 0.8 [0.0, 1.2] | 1.000 | **insufficient_n** |
| error rate | rr vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | lc vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ch vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rule vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ai vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rr vs lc | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ch vs lc | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rule vs lc | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ai vs lc | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |

## E10: Ablation

*Question:* Which components contribute?  
*Scenario:* `e8_mixed.json`, pool `hetero_cpu`, 44.0 s; runs per condition: 3.

| condition | p95 latency (ms) | p99 latency (ms) | error rate | OK throughput (rps) | imbalance CV | overload (s) | switches/min |
|---|---|---|---|---|---|---|---|
| **ai_full** | 862.9 ± 93.9 [754.5, 917.1] | 1101.3 ± 119.8 [962.9, 1170.4] | 0.3014 ± 0.0169 [0.2858, 0.3193] | 490.6 ± 12.3 [476.4, 499.0] | 0.122 ± 0.037 [0.079, 0.145] | 26.3 ± 1.2 [25.0, 27.0] | 8.57 ± 0.00 [8.57, 8.57] |
| **ai_no_context** | 965.2 ± 79.5 [873.4, 1011.1] | 1372.2 ± 266.5 [1114.7, 1646.9] | 0.2967 ± 0.0451 [0.2451, 0.3288] | 494.5 ± 30.9 [476.3, 530.2] | 0.222 ± 0.087 [0.130, 0.303] | 25.3 ± 1.2 [24.0, 26.0] | 8.57 ± 1.71 [6.86, 10.29] |
| **ai_no_ema** | 966.0 ± 94.1 [873.4, 1061.6] | 1739.1 ± 609.6 [1290.4, 2433.2] | 0.3147 ± 0.0123 [0.3005, 0.3227] | 481.8 ± 5.7 [476.9, 488.0] | 0.176 ± 0.023 [0.155, 0.201] | 25.3 ± 1.5 [24.0, 27.0] | 8.57 ± 0.00 [8.57, 8.57] |
| **ai_no_hysteresis** | 947.6 ± 26.5 [917.1, 962.9] | 1846.0 ± 52.4 [1815.7, 1906.5] | 0.2790 ± 0.0438 [0.2284, 0.3065] | 506.2 ± 30.7 [483.8, 541.2] | 0.266 ± 0.055 [0.227, 0.305] | 27.3 ± 1.5 [26.0, 29.0] | 9.14 ± 0.99 [8.57, 10.29] |
| **ai_no_safety** | 906.0 ± 98.6 [792.2, 962.9] | 1416.5 ± 402.4 [1011.1, 1815.7] | 0.3026 ± 0.0216 [0.2784, 0.3203] | 488.8 ± 20.3 [473.4, 511.8] | 0.400 ± 0.083 [0.307, 0.465] | 25.3 ± 2.3 [24.0, 28.0] | 6.86 ± 0.00 [6.86, 6.86] |
| **rule_full** | 850.9 ± 114.6 [718.5, 917.1] | 1174.1 ± 114.4 [1061.6, 1290.4] | 0.2522 ± 0.0770 [0.1635, 0.3020] | 526.1 ± 56.2 [492.0, 591.0] | 0.093 ± 0.051 [0.034, 0.126] | 25.7 ± 1.5 [24.0, 27.0] | 0.00 ± 0.00 [0.00, 0.00] |
| **rule_no_deadband** | 903.9 ± 66.5 [831.8, 962.9] | 1570.1 ± 474.8 [1061.6, 2001.8] | 0.2962 ± 0.0099 [0.2857, 0.3055] | 494.3 ± 7.0 [489.7, 502.3] | 0.150 ± 0.080 [0.060, 0.213] | 25.3 ± 0.6 [25.0, 26.0] | 0.00 ± 0.00 [0.00, 0.00] |
| **rule_no_ema** | 965.2 ± 83.5 [917.1, 1061.6] | 2063.6 ± 434.6 [1729.3, 2554.9] | 0.2800 ± 0.0218 [0.2664, 0.3051] | 504.8 ± 12.8 [490.2, 513.8] | 0.138 ± 0.066 [0.070, 0.201] | 26.3 ± 2.1 [24.0, 28.0] | 0.00 ± 0.00 [0.00, 0.00] |

Paired comparisons for p95 and error rate (candidate − reference; negative is better). All metrics are in `comparisons.csv`. With n = 3 pairs no verdict can be anything but `insufficient_n` (smallest attainable p = 0.250); the differences are descriptive.

| metric | candidate vs reference | mean diff [95% CI] | p (Holm) | verdict |
|---|---|---|---|---|
| p95 latency (ms) | ai_no_context vs ai_full | 102.3 [-43.7, 256.6] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai_no_ema vs ai_full | 103.1 [45.9, 144.6] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai_no_hysteresis vs ai_full | 84.8 [0.0, 208.4] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai_no_safety vs ai_full | 43.1 [-124.9, 208.4] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule_no_deadband vs rule_full | 53.0 [0.0, 113.3] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule_no_ema vs rule_full | 114.4 [0.0, 198.5] | 1.000 | **insufficient_n** |
| error rate | ai_no_context vs ai_full | -0.0047 [-0.0742, 0.0303] | 1.000 | **insufficient_n** |
| error rate | ai_no_ema vs ai_full | 0.0133 [0.0015, 0.0236] | 1.000 | **insufficient_n** |
| error rate | ai_no_hysteresis vs ai_full | -0.0224 [-0.0909, 0.0207] | 1.000 | **insufficient_n** |
| error rate | ai_no_safety vs ai_full | 0.0012 [-0.0206, 0.0344] | 1.000 | **insufficient_n** |
| error rate | rule_no_deadband vs rule_full | 0.0440 [0.0035, 0.1222] | 0.500 | **insufficient_n** |
| error rate | rule_no_ema vs rule_full | 0.0278 [-0.0226, 0.1030] | 0.750 | **insufficient_n** |

## E1: Balanced baseline

*Question:* Does AI avoid unnecessary overhead or switching?  
*Scenario:* `e1_balanced.json`, pool `balanced`, 36 s; runs per condition: 3.

| condition | p95 latency (ms) | p99 latency (ms) | error rate | OK throughput (rps) | imbalance CV |
|---|---|---|---|---|---|
| **rr** | 23.3 ± 1.3 [22.5, 24.8] | 24.5 ± 2.5 [22.5, 27.3] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 300.0 ± 2.9 [296.9, 302.5] | 0.000 ± 0.000 [0.000, 0.000] |
| **lc** | 22.9 ± 0.6 [22.5, 23.6] | 28.0 ± 7.5 [23.6, 36.6] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 300.0 ± 2.9 [296.9, 302.5] | 0.004 ± 0.002 [0.003, 0.006] |
| **swrr** | 22.5 ± 0.0 [22.5, 22.5] | 24.9 ± 2.1 [23.6, 27.3] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 300.0 ± 2.9 [296.9, 302.5] | 0.000 ± 0.000 [0.000, 0.000] |
| **rule** | 22.5 ± 0.0 [22.5, 22.5] | 23.6 ± 0.0 [23.6, 23.6] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 300.0 ± 2.9 [296.9, 302.5] | 0.000 ± 0.000 [0.000, 0.000] |
| **ai** | 23.2 ± 0.6 [22.5, 23.6] | 24.4 ± 0.7 [23.6, 24.8] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 300.0 ± 2.9 [296.9, 302.5] | 0.000 ± 0.000 [0.000, 0.000] |

Paired comparisons for p95 and error rate (candidate − reference; negative is better). All metrics are in `comparisons.csv`. With n = 3 pairs no verdict can be anything but `insufficient_n` (smallest attainable p = 0.250); the differences are descriptive.

| metric | candidate vs reference | mean diff [95% CI] | p (Holm) | verdict |
|---|---|---|---|---|
| p95 latency (ms) | rr vs swrr | 0.8 [0.0, 2.3] | 1.000 | **insufficient_n** |
| p95 latency (ms) | lc vs swrr | 0.4 [0.0, 1.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule vs swrr | 0.0 [0.0, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai vs swrr | 0.7 [0.0, 1.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rr vs lc | 0.4 [0.0, 1.2] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule vs lc | -0.4 [-1.1, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai vs lc | 0.4 [0.0, 1.1] | 1.000 | **insufficient_n** |
| error rate | rr vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | lc vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rule vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ai vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rr vs lc | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rule vs lc | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ai vs lc | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |

## E2: Heterogeneous capacity

*Question:* Does adaptation use capacity better than RR?  
*Scenario:* `e2_heterogeneous.json`, pool `hetero`, 36 s; runs per condition: 3.

| condition | p95 latency (ms) | p99 latency (ms) | error rate | OK throughput (rps) | imbalance CV |
|---|---|---|---|---|---|
| **rr** | 536.2 ± 0.0 [536.2, 536.2] | 536.2 ± 0.0 [536.2, 536.2] | 0.0357 ± 0.0020 [0.0338, 0.0379] | 1252.5 ± 8.7 [1243.3, 1260.6] | 0.464 ± 0.000 [0.464, 0.464] |
| **lc** | 35.5 ± 2.0 [33.2, 36.6] | 42.5 ± 4.1 [38.5, 46.8] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 1298.7 ± 6.2 [1291.9, 1304.2] | 0.357 ± 0.012 [0.344, 0.368] |
| **swrr** | 22.9 ± 0.6 [22.5, 23.6] | 23.6 ± 0.0 [23.6, 23.6] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 1298.8 ± 6.2 [1292.0, 1304.2] | 0.000 ± 0.000 [0.000, 0.000] |
| **rule** | 23.6 ± 1.2 [22.5, 24.8] | 25.7 ± 1.9 [23.6, 27.3] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 1298.8 ± 6.2 [1292.0, 1304.2] | 0.001 ± 0.000 [0.001, 0.001] |
| **ai** | 23.2 ± 0.6 [22.5, 23.6] | 25.7 ± 2.7 [23.6, 28.7] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 1298.8 ± 6.2 [1292.1, 1304.2] | 0.001 ± 0.001 [0.000, 0.001] |

Paired comparisons for p95 and error rate (candidate − reference; negative is better). All metrics are in `comparisons.csv`. With n = 3 pairs no verdict can be anything but `insufficient_n` (smallest attainable p = 0.250); the differences are descriptive.

| metric | candidate vs reference | mean diff [95% CI] | p (Holm) | verdict |
|---|---|---|---|---|
| p95 latency (ms) | rr vs swrr | 513.3 [512.6, 513.7] | 1.000 | **insufficient_n** |
| p95 latency (ms) | lc vs swrr | 12.6 [9.6, 14.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule vs swrr | 0.8 [0.0, 2.3] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai vs swrr | 0.4 [0.0, 1.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rr vs lc | 500.7 [499.6, 503.0] | 0.750 | **insufficient_n** |
| p95 latency (ms) | rule vs lc | -11.9 [-14.1, -9.6] | 0.750 | **insufficient_n** |
| p95 latency (ms) | ai vs lc | -12.3 [-14.1, -9.6] | 0.750 | **insufficient_n** |
| error rate | rr vs swrr | 0.0357 [0.0338, 0.0379] | 1.000 | **insufficient_n** |
| error rate | lc vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rule vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ai vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rr vs lc | 0.0357 [0.0338, 0.0379] | 0.750 | **insufficient_n** |
| error rate | rule vs lc | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ai vs lc | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |

## E3: CPU imbalance

*Question:* Does the controller reduce overload duration?  
*Scenario:* `e3_cpu_imbalance.json`, pool `hetero_cpu`, 36 s; runs per condition: 3.

| condition | p95 latency (ms) | p99 latency (ms) | error rate | OK throughput (rps) | imbalance CV | overload (s) | adaptation (s) |
|---|---|---|---|---|---|---|---|
| **rr** | 52.8 ± 17.0 [33.2, 62.7] | 84.6 ± 34.8 [44.5, 107.2] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 601.0 ± 5.1 [595.3, 605.2] | 0.464 ± 0.000 [0.464, 0.464] | 14.0 ± 12.3 [0.0, 23.0] | 0.0 ± 0.0 [0.0, 0.0] |
| **lc** | 35.5 ± 2.7 [33.2, 38.5] | 46.1 ± 3.4 [42.4, 49.1] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 601.1 ± 5.1 [595.5, 605.3] | 0.518 ± 0.008 [0.510, 0.526] | 0.0 ± 0.0 [0.0, 0.0] | 0.0 ± 0.0 [0.0, 0.0] |
| **swrr** | 327.6 ± 403.0 [72.5, 792.2] | 1084.0 ± 1623.2 [102.1, 2957.6] | 0.0067 ± 0.0116 [0.0000, 0.0201] | 594.9 ± 15.0 [577.6, 604.9] | 0.010 ± 0.017 [0.000, 0.029] | 24.0 ± 0.0 [24.0, 24.0] | – |
| **rule** | 269.9 ± 263.1 [54.1, 563.0] | 643.7 ± 694.8 [88.2, 1422.7] | 0.0002 ± 0.0003 [0.0000, 0.0005] | 600.7 ± 5.5 [594.6, 605.0] | 0.075 ± 0.070 [0.001, 0.139] | 20.7 ± 5.8 [14.0, 24.0] | – |
| **ai** | 285.2 ± 158.7 [158.3, 463.2] | 547.1 ± 221.0 [362.9, 792.2] | 0.0001 ± 0.0001 [0.0000, 0.0002] | 601.0 ± 5.0 [595.5, 605.2] | 0.168 ± 0.188 [0.035, 0.383] | 18.0 ± 10.4 [6.0, 24.0] | 6.0 ± 0.0 [6.0, 6.0] |

Paired comparisons for p95 and error rate (candidate − reference; negative is better). All metrics are in `comparisons.csv`. With n = 3 pairs no verdict can be anything but `insufficient_n` (smallest attainable p = 0.250); the differences are descriptive.

| metric | candidate vs reference | mean diff [95% CI] | p (Holm) | verdict |
|---|---|---|---|---|
| p95 latency (ms) | rr vs swrr | -274.8 [-729.5, -9.9] | 1.000 | **insufficient_n** |
| p95 latency (ms) | lc vs swrr | -292.1 [-757.3, -34.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule vs swrr | -57.8 [-599.7, 490.5] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai vs swrr | -42.5 [-633.9, 345.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rr vs lc | 17.3 [0.0, 27.8] | 0.750 | **insufficient_n** |
| p95 latency (ms) | rule vs lc | 234.3 [20.9, 524.5] | 0.750 | **insufficient_n** |
| p95 latency (ms) | ai vs lc | 249.6 [123.4, 429.9] | 0.750 | **insufficient_n** |
| error rate | rr vs swrr | -0.0067 [-0.0201, 0.0000] | 1.000 | **insufficient_n** |
| error rate | lc vs swrr | -0.0067 [-0.0201, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rule vs swrr | -0.0065 [-0.0201, 0.0005] | 1.000 | **insufficient_n** |
| error rate | ai vs swrr | -0.0066 [-0.0201, 0.0002] | 1.000 | **insufficient_n** |
| error rate | rr vs lc | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rule vs lc | 0.0002 [0.0000, 0.0005] | 1.000 | **insufficient_n** |
| error rate | ai vs lc | 0.0001 [0.0000, 0.0002] | 1.000 | **insufficient_n** |

## E4: Latency imbalance

*Question:* Does the chosen policy reduce tail latency?  
*Scenario:* `e4_latency_imbalance.json`, pool `hetero`, 36 s; runs per condition: 3.

| condition | p95 latency (ms) | p99 latency (ms) | error rate | OK throughput (rps) | imbalance CV | overload (s) | adaptation (s) |
|---|---|---|---|---|---|---|---|
| **rr** | 684.3 ± 0.0 [684.3, 684.3] | 684.3 ± 0.0 [684.3, 684.3] | 0.1473 ± 0.0020 [0.1450, 0.1489] | 509.3 ± 3.2 [505.8, 512.0] | 0.464 ± 0.000 [0.464, 0.464] | 24.0 ± 0.0 [24.0, 24.0] | – |
| **lc** | 174.6 ± 0.0 [174.6, 174.6] | 174.6 ± 0.0 [174.6, 174.6] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 601.0 ± 5.1 [595.2, 605.2] | 0.764 ± 0.000 [0.763, 0.764] | 24.0 ± 0.0 [24.0, 24.0] | – |
| **swrr** | 684.3 ± 0.0 [684.3, 684.3] | 684.3 ± 0.0 [684.3, 684.3] | 0.1472 ± 0.0021 [0.1449, 0.1491] | 509.3 ± 3.2 [505.7, 511.9] | 0.000 ± 0.000 [0.000, 0.000] | 24.0 ± 0.0 [24.0, 24.0] | – |
| **rule** | 673.5 ± 18.8 [651.7, 684.3] | 684.3 ± 0.0 [684.3, 684.3] | 0.0652 ± 0.0014 [0.0636, 0.0661] | 559.7 ± 4.0 [555.4, 563.2] | 0.176 ± 0.002 [0.175, 0.178] | 24.0 ± 0.0 [24.0, 24.0] | – |
| **ai** | 684.3 ± 0.0 [684.3, 684.3] | 684.3 ± 0.0 [684.3, 684.3] | 0.1151 ± 0.0416 [0.0671, 0.1397] | 528.3 ± 20.4 [515.6, 551.8] | 0.165 ± 0.232 [0.020, 0.433] | 24.0 ± 0.0 [24.0, 24.0] | – |

Paired comparisons for p95 and error rate (candidate − reference; negative is better). All metrics are in `comparisons.csv`. With n = 3 pairs no verdict can be anything but `insufficient_n` (smallest attainable p = 0.250); the differences are descriptive.

| metric | candidate vs reference | mean diff [95% CI] | p (Holm) | verdict |
|---|---|---|---|---|
| p95 latency (ms) | rr vs swrr | 0.0 [0.0, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | lc vs swrr | -509.8 [-509.8, -509.8] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule vs swrr | -10.9 [-32.6, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai vs swrr | 0.0 [0.0, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rr vs lc | 509.8 [509.8, 509.8] | 0.750 | **insufficient_n** |
| p95 latency (ms) | rule vs lc | 498.9 [477.2, 509.8] | 0.750 | **insufficient_n** |
| p95 latency (ms) | ai vs lc | 509.8 [509.8, 509.8] | 0.750 | **insufficient_n** |
| error rate | rr vs swrr | 0.0000 [-0.0002, 0.0001] | 1.000 | **insufficient_n** |
| error rate | lc vs swrr | -0.1472 [-0.1491, -0.1449] | 1.000 | **insufficient_n** |
| error rate | rule vs swrr | -0.0820 [-0.0830, -0.0813] | 1.000 | **insufficient_n** |
| error rate | ai vs swrr | -0.0321 [-0.0778, -0.0092] | 1.000 | **insufficient_n** |
| error rate | rr vs lc | 0.1473 [0.1450, 0.1489] | 0.750 | **insufficient_n** |
| error rate | rule vs lc | 0.0652 [0.0636, 0.0661] | 0.750 | **insufficient_n** |
| error rate | ai vs lc | 0.1151 [0.0671, 0.1397] | 0.750 | **insufficient_n** |

## E5: Long-running requests

*Question:* Does LC or another strategy perform better?  
*Scenario:* `e5_long_requests.json`, pool `hetero`, 36 s; runs per condition: 3.

| condition | p95 latency (ms) | p99 latency (ms) | error rate | OK throughput (rps) | imbalance CV |
|---|---|---|---|---|---|
| **rr** | 563.4 ± 27.5 [536.2, 591.1] | 947.6 ± 26.5 [917.1, 962.9] | 0.1345 ± 0.0034 [0.1317, 0.1383] | 345.8 ± 4.3 [341.7, 350.3] | 0.464 ± 0.000 [0.463, 0.464] |
| **lc** | 463.2 ± 0.0 [463.2, 463.2] | 610.9 ± 17.1 [591.1, 620.7] | 0.0000 ± 0.0001 [0.0000, 0.0001] | 399.6 ± 4.2 [396.1, 404.2] | 0.149 ± 0.036 [0.116, 0.188] |
| **swrr** | 448.5 ± 12.7 [441.1, 463.2] | 563.4 ± 27.5 [536.2, 591.1] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 399.8 ± 3.8 [397.0, 404.2] | 0.000 ± 0.000 [0.000, 0.000] |
| **rule** | 448.5 ± 12.7 [441.1, 463.2] | 554.5 ± 31.7 [536.2, 591.1] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 399.6 ± 4.1 [396.3, 404.2] | 0.001 ± 0.000 [0.001, 0.001] |
| **ai** | 527.7 ± 14.7 [510.7, 536.2] | 902.5 ± 25.2 [873.4, 917.1] | 0.0377 ± 0.0052 [0.0317, 0.0415] | 384.2 ± 6.5 [378.9, 391.5] | 0.158 ± 0.035 [0.118, 0.183] |

Paired comparisons for p95 and error rate (candidate − reference; negative is better). All metrics are in `comparisons.csv`. With n = 3 pairs no verdict can be anything but `insufficient_n` (smallest attainable p = 0.250); the differences are descriptive.

| metric | candidate vs reference | mean diff [95% CI] | p (Holm) | verdict |
|---|---|---|---|---|
| p95 latency (ms) | rr vs swrr | 115.0 [73.0, 150.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | lc vs swrr | 14.7 [0.0, 22.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule vs swrr | 0.0 [0.0, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai vs swrr | 79.2 [47.5, 95.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rr vs lc | 100.3 [73.0, 128.0] | 0.750 | **insufficient_n** |
| p95 latency (ms) | rule vs lc | -14.7 [-22.1, 0.0] | 0.750 | **insufficient_n** |
| p95 latency (ms) | ai vs lc | 64.5 [47.5, 73.0] | 0.750 | **insufficient_n** |
| error rate | rr vs swrr | 0.1345 [0.1317, 0.1383] | 1.000 | **insufficient_n** |
| error rate | lc vs swrr | 0.0000 [0.0000, 0.0001] | 1.000 | **insufficient_n** |
| error rate | rule vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ai vs swrr | 0.0377 [0.0317, 0.0415] | 1.000 | **insufficient_n** |
| error rate | rr vs lc | 0.1345 [0.1317, 0.1383] | 0.750 | **insufficient_n** |
| error rate | rule vs lc | -0.0000 [-0.0001, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ai vs lc | 0.0377 [0.0316, 0.0415] | 0.750 | **insufficient_n** |

## E6: Flash crowd

*Question:* How quickly does the system adapt?  
*Scenario:* `e6_flash_crowd.json`, pool `hetero`, 36 s; runs per condition: 3.

| condition | p95 latency (ms) | p99 latency (ms) | error rate | OK throughput (rps) | imbalance CV | overload (s) | adaptation (s) |
|---|---|---|---|---|---|---|---|
| **rr** | 536.2 ± 0.0 [536.2, 536.2] | 536.2 ± 0.0 [536.2, 536.2] | 0.0294 ± 0.0027 [0.0274, 0.0325] | 945.1 ± 8.9 [938.6, 955.3] | 0.464 ± 0.000 [0.463, 0.464] | 11.3 ± 0.6 [11.0, 12.0] | – |
| **lc** | 32.2 ± 0.9 [31.6, 33.2] | 39.1 ± 1.1 [38.5, 40.4] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 973.8 ± 7.3 [969.1, 982.2] | 0.378 ± 0.001 [0.378, 0.379] | 0.0 ± 0.0 [0.0, 0.0] | 0.0 ± 0.0 [0.0, 0.0] |
| **swrr** | 22.9 ± 0.6 [22.5, 23.6] | 23.6 ± 0.0 [23.6, 23.6] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 973.8 ± 7.3 [969.1, 982.2] | 0.000 ± 0.000 [0.000, 0.000] | 0.0 ± 0.0 [0.0, 0.0] | 0.0 ± 0.0 [0.0, 0.0] |
| **rule** | 23.6 ± 1.2 [22.5, 24.8] | 24.9 ± 2.1 [23.6, 27.3] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 973.8 ± 7.3 [969.1, 982.2] | 0.001 ± 0.000 [0.001, 0.001] | 0.0 ± 0.0 [0.0, 0.0] | 0.0 ± 0.0 [0.0, 0.0] |
| **ai** | 23.2 ± 0.6 [22.5, 23.6] | 26.1 ± 1.3 [24.8, 27.3] | 0.0000 ± 0.0000 [0.0000, 0.0000] | 973.8 ± 7.3 [969.1, 982.2] | 0.001 ± 0.001 [0.000, 0.001] | 0.0 ± 0.0 [0.0, 0.0] | 0.0 ± 0.0 [0.0, 0.0] |

Paired comparisons for p95 and error rate (candidate − reference; negative is better). All metrics are in `comparisons.csv`. With n = 3 pairs no verdict can be anything but `insufficient_n` (smallest attainable p = 0.250); the differences are descriptive.

| metric | candidate vs reference | mean diff [95% CI] | p (Holm) | verdict |
|---|---|---|---|---|
| p95 latency (ms) | rr vs swrr | 513.3 [512.6, 513.7] | 1.000 | **insufficient_n** |
| p95 latency (ms) | lc vs swrr | 9.3 [9.2, 9.6] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule vs swrr | 0.8 [0.0, 1.2] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai vs swrr | 0.4 [0.0, 1.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rr vs lc | 504.0 [503.0, 504.5] | 0.750 | **insufficient_n** |
| p95 latency (ms) | rule vs lc | -8.5 [-9.2, -8.0] | 0.750 | **insufficient_n** |
| p95 latency (ms) | ai vs lc | -8.9 [-9.6, -8.0] | 0.750 | **insufficient_n** |
| error rate | rr vs swrr | 0.0294 [0.0274, 0.0325] | 1.000 | **insufficient_n** |
| error rate | lc vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rule vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ai vs swrr | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rr vs lc | 0.0294 [0.0274, 0.0325] | 0.750 | **insufficient_n** |
| error rate | rule vs lc | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ai vs lc | 0.0000 [0.0000, 0.0000] | 1.000 | **insufficient_n** |

## E7: Failure / recovery

*Question:* What are detection and recovery times?  
*Scenario:* `e7_failure_recovery.json`, pool `hetero`, 36 s; runs per condition: 3.

| condition | p95 latency (ms) | p99 latency (ms) | error rate | OK throughput (rps) | imbalance CV | detection (s) | re-entry: marked up (s) | re-entry: share restored (s) | overload (s) |
|---|---|---|---|---|---|---|---|---|---|
| **rr** | 356.2 ± 149.5 [212.2, 510.7] | 383.7 ± 151.1 [233.9, 536.2] | 0.0015 ± 0.0026 [0.0001, 0.0045] | 600.2 ± 4.1 [595.5, 602.6] | 0.629 ± 0.001 [0.629, 0.630] | 1.00 ± 0.00 [1.00, 1.00] | 1.00 ± 0.00 [1.00, 1.00] | 3.00 ± 0.00 [3.00, 3.00] | 11.7 ± 0.6 [11.0, 12.0] |
| **lc** | 23.6 ± 0.0 [23.6, 23.6] | 27.8 ± 0.8 [27.3, 28.7] | 0.0001 ± 0.0000 [0.0001, 0.0001] | 601.1 ± 5.1 [595.5, 605.3] | 0.494 ± 0.000 [0.493, 0.494] | 1.00 ± 0.00 [1.00, 1.00] | 1.00 ± 0.00 [1.00, 1.00] | 3.00 ± 0.00 [3.00, 3.00] | 0.0 ± 0.0 [0.0, 0.0] |
| **swrr** | 22.9 ± 0.6 [22.5, 23.6] | 24.4 ± 1.4 [23.6, 26.0] | 0.0001 ± 0.0000 [0.0001, 0.0001] | 601.1 ± 5.1 [595.5, 605.3] | 0.000 ± 0.000 [0.000, 0.000] | 1.00 ± 0.00 [1.00, 1.00] | 1.00 ± 0.00 [1.00, 1.00] | 3.00 ± 0.00 [3.00, 3.00] | 0.0 ± 0.0 [0.0, 0.0] |
| **rule** | 22.9 ± 0.6 [22.5, 23.6] | 30.6 ± 12.1 [23.6, 44.5] | 0.0001 ± 0.0000 [0.0001, 0.0001] | 601.1 ± 5.1 [595.5, 605.3] | 0.007 ± 0.001 [0.006, 0.008] | 1.00 ± 0.00 [1.00, 1.00] | 1.00 ± 0.00 [1.00, 1.00] | 4.00 ± 0.00 [4.00, 4.00] | 0.7 ± 1.2 [0.0, 2.0] |
| **ai** | 23.2 ± 0.6 [22.5, 23.6] | 25.7 ± 1.9 [23.6, 27.3] | 0.0001 ± 0.0000 [0.0001, 0.0001] | 601.1 ± 5.1 [595.4, 605.3] | 0.005 ± 0.004 [0.000, 0.008] | 1.00 ± 0.00 [1.00, 1.00] | 1.00 ± 0.00 [1.00, 1.00] | 3.67 ± 0.58 [3.00, 4.00] | 0.0 ± 0.0 [0.0, 0.0] |

Paired comparisons for p95 and error rate (candidate − reference; negative is better). All metrics are in `comparisons.csv`. With n = 3 pairs no verdict can be anything but `insufficient_n` (smallest attainable p = 0.250); the differences are descriptive.

| metric | candidate vs reference | mean diff [95% CI] | p (Holm) | verdict |
|---|---|---|---|---|
| p95 latency (ms) | rr vs swrr | 333.3 [189.7, 488.2] | 1.000 | **insufficient_n** |
| p95 latency (ms) | lc vs swrr | 0.7 [0.0, 1.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule vs swrr | 0.0 [-1.1, 1.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai vs swrr | 0.4 [0.0, 1.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rr vs lc | 332.5 [188.6, 487.0] | 0.750 | **insufficient_n** |
| p95 latency (ms) | rule vs lc | -0.7 [-1.1, 0.0] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai vs lc | -0.4 [-1.1, 0.0] | 1.000 | **insufficient_n** |
| error rate | rr vs swrr | 0.0015 [0.0000, 0.0044] | 1.000 | **insufficient_n** |
| error rate | lc vs swrr | 0.0000 [0.0000, 0.0001] | 1.000 | **insufficient_n** |
| error rate | rule vs swrr | 0.0000 [0.0000, 0.0001] | 1.000 | **insufficient_n** |
| error rate | ai vs swrr | 0.0000 [-0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | rr vs lc | 0.0014 [-0.0001, 0.0044] | 1.000 | **insufficient_n** |
| error rate | rule vs lc | -0.0000 [-0.0000, 0.0000] | 1.000 | **insufficient_n** |
| error rate | ai vs lc | -0.0000 [-0.0001, 0.0000] | 1.000 | **insufficient_n** |

## E8: Mixed workload

*Question:* Does learned selection generalize?  
*Scenario:* `e8_mixed.json`, pool `hetero_cpu`, 44.0 s; runs per condition: 3.

| condition | p95 latency (ms) | p99 latency (ms) | error rate | OK throughput (rps) | imbalance CV | overload (s) | adaptation (s) |
|---|---|---|---|---|---|---|---|
| **rr** | 747.9 ± 113.7 [651.7, 873.4] | 909.8 ± 236.0 [651.7, 1114.7] | 0.3141 ± 0.0171 [0.2971, 0.3312] | 484.4 ± 17.4 [466.8, 501.7] | 0.586 ± 0.001 [0.585, 0.586] | 27.3 ± 1.5 [26.0, 29.0] | – |
| **lc** | 756.9 ± 73.8 [684.3, 831.8] | 1048.1 ± 108.6 [962.9, 1170.4] | 0.2710 ± 0.0168 [0.2518, 0.2825] | 511.5 ± 11.5 [502.3, 524.4] | 0.504 ± 0.038 [0.460, 0.528] | 24.0 ± 0.0 [24.0, 24.0] | – |
| **swrr** | 995.8 ± 57.0 [962.9, 1061.6] | 2140.9 ± 648.3 [1422.7, 2682.6] | 0.3160 ± 0.0103 [0.3048, 0.3251] | 480.5 ± 6.8 [475.0, 488.1] | 0.094 ± 0.085 [0.034, 0.155] | 26.3 ± 0.6 [26.0, 27.0] | – |
| **rule** | 1045.6 ± 59.8 [1011.1, 1114.7] | 2298.0 ± 619.2 [1729.3, 2957.6] | 0.3042 ± 0.0223 [0.2786, 0.3197] | 485.0 ± 11.2 [478.4, 497.9] | 0.176 ± 0.000 [0.176, 0.176] | 26.3 ± 1.2 [25.0, 27.0] | – |
| **ai** | 979.0 ± 27.8 [962.9, 1011.1] | 1900.9 ± 367.2 [1493.8, 2207.0] | 0.3172 ± 0.0076 [0.3086, 0.3229] | 479.3 ± 8.9 [471.7, 489.1] | 0.226 ± 0.016 [0.211, 0.242] | 26.0 ± 1.0 [25.0, 27.0] | – |

Paired comparisons for p95 and error rate (candidate − reference; negative is better). All metrics are in `comparisons.csv`. With n = 3 pairs no verdict can be anything but `insufficient_n` (smallest attainable p = 0.250); the differences are descriptive.

| metric | candidate vs reference | mean diff [95% CI] | p (Holm) | verdict |
|---|---|---|---|---|
| p95 latency (ms) | rr vs swrr | -247.9 [-311.2, -188.2] | 1.000 | **insufficient_n** |
| p95 latency (ms) | lc vs swrr | -238.9 [-307.1, -131.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule vs swrr | 49.8 [-50.6, 151.8] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai vs swrr | -16.9 [-98.7, 48.1] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rr vs lc | -9.0 [-180.1, 118.9] | 1.000 | **insufficient_n** |
| p95 latency (ms) | rule vs lc | 288.7 [179.3, 430.4] | 0.750 | **insufficient_n** |
| p95 latency (ms) | ai vs lc | 222.1 [131.1, 326.7] | 0.750 | **insufficient_n** |
| error rate | rr vs swrr | -0.0019 [-0.0280, 0.0130] | 1.000 | **insufficient_n** |
| error rate | lc vs swrr | -0.0450 [-0.0530, -0.0395] | 1.000 | **insufficient_n** |
| error rate | rule vs swrr | -0.0118 [-0.0396, 0.0096] | 1.000 | **insufficient_n** |
| error rate | ai vs swrr | 0.0012 [-0.0165, 0.0153] | 1.000 | **insufficient_n** |
| error rate | rr vs lc | 0.0431 [0.0146, 0.0624] | 0.750 | **insufficient_n** |
| error rate | rule vs lc | 0.0332 [-0.0001, 0.0626] | 0.750 | **insufficient_n** |
| error rate | ai vs lc | 0.0462 [0.0261, 0.0682] | 0.750 | **insufficient_n** |

## E9: Control-interval sweep

*Question:* Tradeoff between responsiveness and instability?  
*Scenario:* `e9_mixed_long.json`, pool `hetero_cpu`, 77.0 s; runs per condition: 3.

| condition | p95 latency (ms) | p99 latency (ms) | error rate | OK throughput (rps) | imbalance CV | overload (s) | switches/min |
|---|---|---|---|---|---|---|---|
| **swrr** | 902.2 ± 199.6 [718.5, 1114.7] | 1342.1 ± 573.5 [962.9, 2001.8] | 0.2704 ± 0.0565 [0.2054, 0.3081] | 499.3 ± 40.8 [473.6, 546.3] | 0.038 ± 0.065 [0.000, 0.112] | 45.0 ± 2.0 [43.0, 47.0] | 0.00 ± 0.00 [0.00, 0.00] |
| **ai@10s** | 873.4 ± 0.0 [873.4, 873.4] | 1174.1 ± 114.4 [1061.6, 1290.4] | 0.2884 ± 0.0020 [0.2866, 0.2905] | 486.9 ± 1.2 [485.5, 487.8] | 0.143 ± 0.067 [0.067, 0.192] | 45.0 ± 1.7 [43.0, 46.0] | 7.06 ± 0.88 [6.18, 7.94] |
| **ai@1s** | 968.4 ± 128.6 [873.4, 1114.7] | 1540.4 ± 487.2 [1229.0, 2101.9] | 0.2984 ± 0.0096 [0.2891, 0.3083] | 480.0 ± 5.7 [473.4, 483.7] | 0.071 ± 0.041 [0.024, 0.101] | 47.7 ± 1.5 [46.0, 49.0] | 7.06 ± 2.33 [4.41, 8.82] |
| **ai@30s** | 995.0 ± 27.8 [962.9, 1011.1] | 1573.5 ± 153.4 [1422.7, 1729.3] | 0.2829 ± 0.0362 [0.2419, 0.3103] | 490.6 ± 25.5 [469.3, 518.9] | 0.100 ± 0.020 [0.084, 0.123] | 46.7 ± 2.5 [44.0, 49.0] | 2.35 ± 1.02 [1.76, 3.53] |
| **ai@5s** | 947.6 ± 26.5 [917.1, 962.9] | 1299.6 ± 189.7 [1114.7, 1493.8] | 0.2959 ± 0.0060 [0.2916, 0.3028] | 481.7 ± 4.3 [477.2, 485.9] | 0.141 ± 0.068 [0.086, 0.217] | 45.0 ± 3.0 [42.0, 48.0] | 7.06 ± 3.85 [2.65, 9.71] |
| **ai@60s** | 1044.8 ± 29.2 [1011.1, 1061.6] | 1524.9 ± 177.0 [1422.7, 1729.3] | 0.3046 ± 0.0089 [0.2975, 0.3146] | 475.8 ± 4.8 [471.2, 480.9] | 0.061 ± 0.043 [0.021, 0.105] | 45.0 ± 3.6 [42.0, 49.0] | 1.18 ± 1.02 [0.00, 1.76] |

Paired comparisons for p95 and error rate (candidate − reference; negative is better). All metrics are in `comparisons.csv`. With n = 3 pairs no verdict can be anything but `insufficient_n` (smallest attainable p = 0.250); the differences are descriptive.

| metric | candidate vs reference | mean diff [95% CI] | p (Holm) | verdict |
|---|---|---|---|---|
| p95 latency (ms) | ai@10s vs swrr | -28.8 [-241.3, 154.9] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai@1s vs swrr | 66.2 [0.0, 198.5] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai@30s vs swrr | 92.8 [-151.8, 292.5] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai@5s vs swrr | 45.4 [-151.8, 198.5] | 1.000 | **insufficient_n** |
| p95 latency (ms) | ai@60s vs swrr | 142.6 [-53.1, 292.5] | 1.000 | **insufficient_n** |
| error rate | ai@10s vs swrr | 0.0180 [-0.0200, 0.0851] | 1.000 | **insufficient_n** |
| error rate | ai@1s vs swrr | 0.0280 [-0.0086, 0.0923] | 1.000 | **insufficient_n** |
| error rate | ai@30s vs swrr | 0.0126 [-0.0661, 0.0912] | 1.000 | **insufficient_n** |
| error rate | ai@5s vs swrr | 0.0255 [-0.0062, 0.0880] | 1.000 | **insufficient_n** |
| error rate | ai@60s vs swrr | 0.0342 [-0.0106, 0.1093] | 1.000 | **insufficient_n** |

## Controller overhead and switching

| experiment | condition | decisions (accepted) | switches/min | decision latency avg / max (ms) | agent failures | safety fallbacks | agent CPU % / RAM MB | LB CPU % / RAM MB | request-path overhead p50 / p95 (ms) |
|---|---|---|---|---|---|---|---|---|---|
| A1 | rr | – | 0.00 | – | – | – | – | 45.8 / 22 | 0.000 / 0.000 |
| A1 | lc | – | 0.00 | – | – | – | – | 52.2 / 22 | 0.000 / 0.000 |
| A1 | swrr | – | 0.00 | – | – | – | – | 57.5 / 22 | 0.000 / 0.000 |
| A1 | rule | – | 0.00 | – | – | – | – | 39.5 / 22 | 0.000 / 0.000 |
| A1 | ai | 8.0 (1.0) | 0.00 | 18.7 / 26.3 | 0.0 | 0.0 | 0.4 / 38 | 63.5 / 23 | 0.000 / 0.000 |
| E10 | ai_full | 6.0 (4.0) | 8.57 | 13.4 / 20.7 | 0.0 | 2.0 | 0.2 / 38 | 91.4 / 62 | 0.000 / 0.000 |
| E10 | ai_no_context | 5.3 (3.3) | 8.57 | 18.7 / 50.3 | 0.0 | 2.0 | 0.1 / 38 | 84.2 / 67 | 0.000 / 0.000 |
| E10 | ai_no_ema | 5.7 (3.7) | 8.57 | 13.2 / 22.6 | 0.0 | 2.0 | 0.2 / 38 | 100.0 / 70 | 0.000 / 0.000 |
| E10 | ai_no_hysteresis | 5.7 (5.3) | 9.14 | 13.2 / 19.2 | 0.0 | 2.0 | 0.1 / 38 | 79.9 / 71 | 0.000 / 0.000 |
| E10 | ai_no_safety | 8.0 (5.0) | 6.86 | 14.2 / 21.9 | 0.0 | 0.0 | 0.2 / 38 | 88.7 / 61 | 0.000 / 0.000 |
| E1 | rr | – | 0.00 | – | – | – | – | 34.5 / 21 | 0.000 / 0.000 |
| E1 | lc | – | 0.00 | – | – | – | – | 33.4 / 22 | 0.000 / 0.000 |
| E1 | swrr | – | 0.00 | – | – | – | – | 33.8 / 21 | 0.000 / 0.000 |
| E1 | rule | – | 0.00 | – | – | – | – | 34.8 / 21 | 0.000 / 0.000 |
| E1 | ai | 8.3 (0.7) | 0.00 | 17.3 / 27.5 | 0.0 | 0.0 | 0.4 / 38 | 38.8 / 21 | 0.000 / 0.000 |
| E2 | rr | – | 0.00 | – | – | – | – | 142.7 / 51 | 0.000 / 0.000 |
| E2 | lc | – | 0.00 | – | – | – | – | 171.0 / 28 | 0.000 / 0.000 |
| E2 | swrr | – | 0.00 | – | – | – | – | 127.7 / 30 | 0.000 / 0.000 |
| E2 | rule | – | 0.00 | – | – | – | – | 139.3 / 28 | 0.000 / 0.000 |
| E2 | ai | 8.3 (0.7) | 0.00 | 18.1 / 30.6 | 0.0 | 0.0 | 0.5 / 38 | 138.2 / 28 | 0.000 / 0.000 |
| E3 | rr | – | 0.00 | – | – | – | – | 74.5 / 24 | 0.000 / 0.000 |
| E3 | lc | – | 0.00 | – | – | – | – | 69.1 / 24 | 0.000 / 0.000 |
| E3 | swrr | – | 0.00 | – | – | – | – | 76.8 / 30 | 0.000 / 0.000 |
| E3 | rule | – | 0.00 | – | – | – | – | 61.3 / 31 | 0.000 / 0.000 |
| E3 | ai | 8.0 (2.0) | 2.96 | 17.0 / 28.5 | 0.0 | 0.0 | 0.3 / 38 | 71.4 / 33 | 0.000 / 0.000 |
| E4 | rr | – | 0.00 | – | – | – | – | 67.6 / 33 | 0.000 / 0.000 |
| E4 | lc | – | 0.00 | – | – | – | – | 74.1 / 24 | 0.000 / 0.000 |
| E4 | swrr | – | 0.00 | – | – | – | – | 65.3 / 33 | 0.000 / 0.000 |
| E4 | rule | – | 0.00 | – | – | – | – | 70.9 / 32 | 0.000 / 0.000 |
| E4 | ai | 5.7 (3.0) | 8.15 | 11.6 / 19.3 | 0.0 | 1.3 | 0.1 / 38 | 65.5 / 33 | 0.000 / 0.000 |
| E5 | rr | – | 0.00 | – | – | – | – | 45.9 / 34 | 0.000 / 0.000 |
| E5 | lc | – | 0.00 | – | – | – | – | 46.3 / 28 | 0.000 / 0.000 |
| E5 | swrr | – | 0.00 | – | – | – | – | 38.7 / 29 | 0.000 / 0.000 |
| E5 | rule | – | 0.00 | – | – | – | – | 48.8 / 29 | 0.000 / 0.000 |
| E5 | ai | 6.0 (3.7) | 7.41 | 12.9 / 22.7 | 0.0 | 0.7 | 0.2 / 38 | 39.5 / 31 | 0.000 / 0.000 |
| E6 | rr | – | 0.00 | – | – | – | – | 119.4 / 40 | 0.000 / 0.000 |
| E6 | lc | – | 0.00 | – | – | – | – | 116.1 / 24 | 0.000 / 0.000 |
| E6 | swrr | – | 0.00 | – | – | – | – | 109.9 / 24 | 0.000 / 0.000 |
| E6 | rule | – | 0.00 | – | – | – | – | 119.2 / 24 | 0.000 / 0.000 |
| E6 | ai | 8.3 (0.7) | 0.00 | 15.2 / 20.5 | 0.0 | 0.0 | 0.4 / 38 | 114.2 / 24 | 0.000 / 0.000 |
| E7 | rr | – | 0.00 | – | – | – | – | 61.7 / 31 | 0.000 / 0.000 |
| E7 | lc | – | 0.00 | – | – | – | – | 74.4 / 23 | 0.000 / 0.000 |
| E7 | swrr | – | 0.00 | – | – | – | – | 79.9 / 24 | 0.000 / 0.000 |
| E7 | rule | – | 0.00 | – | – | – | – | 74.3 / 25 | 0.000 / 0.000 |
| E7 | ai | 8.3 (0.7) | 0.00 | 17.7 / 24.9 | 0.0 | 0.0 | 0.4 / 38 | 81.0 / 24 | 0.000 / 0.000 |
| E8 | rr | – | 0.00 | – | – | – | – | 81.6 / 50 | 0.000 / 0.000 |
| E8 | lc | – | 0.00 | – | – | – | – | 87.5 / 56 | 0.000 / 0.000 |
| E8 | swrr | – | 0.00 | – | – | – | – | 95.9 / 71 | 0.000 / 0.000 |
| E8 | rule | – | 0.00 | – | – | – | – | 87.4 / 74 | 0.000 / 0.000 |
| E8 | ai | 5.7 (4.0) | 8.57 | 13.0 / 29.1 | 0.0 | 2.0 | 0.1 / 38 | 80.4 / 70 | 0.000 / 0.000 |
| E9 | swrr | – | 0.00 | – | – | – | – | 73.8 / 65 | 0.000 / 0.000 |
| E9 | ai@10s | 7.0 (5.3) | 7.06 | 13.1 / 24.7 | 0.0 | 3.7 | 0.1 / 38 | 88.1 / 66 | 0.000 / 0.000 |
| E9 | ai@1s | 38.7 (8.7) | 7.06 | 21.5 / 389.4 | 0.0 | 3.3 | 0.4 / 38 | 91.3 / 71 | 0.000 / 0.000 |
| E9 | ai@30s | 3.7 (1.3) | 2.35 | 8.6 / 13.3 | 0.0 | 1.3 | 0.0 / 38 | 75.2 / 73 | 0.000 / 0.000 |
| E9 | ai@5s | 11.3 (7.0) | 7.06 | 11.3 / 19.0 | 0.0 | 2.7 | 0.2 / 38 | 75.0 / 72 | 0.000 / 0.000 |
| E9 | ai@60s | 2.7 (0.7) | 1.18 | 9.6 / 11.9 | 0.0 | 0.7 | 0.0 / 38 | 71.1 / 74 | 0.000 / 0.000 |

