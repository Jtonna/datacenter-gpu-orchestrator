# CR-9: forced profiles the fit check would refuse

Checked on paper on 21 November against the running configuration, before
any deployment of CR-9. With CR-9 the setting `forced_fit_check: reject`
also applies when workers are agents: a profile forced onto machines that
cannot hold it is refused before anything starts, instead of being tried
and failing.

- 274 profiles, 222 of them forced onto named machines.
- 103 would be refused. All are cells of the generated benchmark matrix
  (and two hand-made profiles) whose need exceeds the usable memory of the
  machines they are forced onto.
- No scenario profile (`sc-*`), no phase-4 profile (`p4-*`) and no load
  profile of the smoke suite is refused.
- Usable memory at the time: about 12 500 MiB per console (available memory
  minus the tier's headroom).

## Borderline: refused by less than 100 MiB

These may load today and would be refused after CR-9. Their need is an
estimate with the 5% margin; a need is only "measured" for a profile that
has been loaded once.

| Profiles | Over by | With a measured need |
|---|---|---|
| `bench-gemma12b-1a/1b/1c-8192`, `m-gemma-8k-1ps5` | 47 to 82 MiB | They pass. A profile with the same file and context has a measured need of 7 850 MiB against an estimate of 12 040 MiB; the estimate is far too high for this model |
| `bench-deepseek-v2-lite-1a/1b/1c-8192` | 52 to 87 MiB | Unknown: no profile with this file and context has been measured. The estimate without margin (12 045 MiB) is below the usable memory, so it passes if the real need is not above the estimate |
| `bench-llama33-70b-q2-3-32768` | 68 MiB on one console's share | Unknown: not measured |

How to settle them before CR-9 is deployed: load each of these profiles
once on the current release (they are not refused today). The scheduler
then stores the measured need for that profile and the check uses it
without the margin.

Passes with little room: `m-gemma-48k` (215 MiB spare on one console).

## Full list

| Profile | Machines | Need with margin (MiB) | Usable (MiB) | Over by (MiB) | Fails on |
|---|---|---|---|---|---|
| bench-deepseek-v2-lite-1a-32768 | 1 | 19451 | 12598 | 6853 | total |
| bench-deepseek-v2-lite-1a-8192 | 1 | 12647 | 12598 | 49 | total |
| bench-deepseek-v2-lite-1b-32768 | 1 | 19451 | 12559 | 6892 | total |
| bench-deepseek-v2-lite-1b-8192 | 1 | 12647 | 12559 | 88 | total |
| bench-deepseek-v2-lite-1c-32768 | 1 | 19451 | 12571 | 6880 | total |
| bench-deepseek-v2-lite-1c-8192 | 1 | 12647 | 12571 | 76 | total |
| bench-devstral24b-1a-2048 | 1 | 14691 | 12598 | 2093 | total |
| bench-devstral24b-1a-32768 | 1 | 19731 | 12598 | 7133 | total |
| bench-devstral24b-1a-8192 | 1 | 15699 | 12598 | 3101 | total |
| bench-devstral24b-1b-2048 | 1 | 14691 | 12559 | 2132 | total |
| bench-devstral24b-1b-32768 | 1 | 19731 | 12559 | 7172 | total |
| bench-devstral24b-1b-8192 | 1 | 15699 | 12559 | 3140 | total |
| bench-devstral24b-1c-2048 | 1 | 14691 | 12571 | 2120 | total |
| bench-devstral24b-1c-32768 | 1 | 19731 | 12571 | 7160 | total |
| bench-devstral24b-1c-8192 | 1 | 15699 | 12571 | 3128 | total |
| bench-gemma12b-1a-32768 | 1 | 29173 | 12598 | 16575 | total |
| bench-gemma12b-1a-8192 | 1 | 12642 | 12598 | 44 | total |
| bench-gemma12b-1b-32768 | 1 | 29173 | 12559 | 16614 | total |
| bench-gemma12b-1b-8192 | 1 | 12642 | 12559 | 83 | total |
| bench-gemma12b-1c-32768 | 1 | 29173 | 12571 | 16602 | total |
| bench-gemma12b-1c-8192 | 1 | 12642 | 12571 | 71 | total |
| bench-gemma12b-2-32768 | 2 | 29173 | 25157 | 4016 | total |
| bench-glm4-32b-1a-2048 | 1 | 19836 | 12598 | 7238 | total |
| bench-glm4-32b-1a-32768 | 1 | 21757 | 12598 | 9159 | total |
| bench-glm4-32b-1a-8192 | 1 | 20220 | 12598 | 7622 | total |
| bench-glm4-32b-1b-2048 | 1 | 19836 | 12559 | 7277 | total |
| bench-glm4-32b-1b-32768 | 1 | 21757 | 12559 | 9198 | total |
| bench-glm4-32b-1b-8192 | 1 | 20220 | 12559 | 7661 | total |
| bench-glm4-32b-1c-2048 | 1 | 19836 | 12571 | 7265 | total |
| bench-glm4-32b-1c-32768 | 1 | 21757 | 12571 | 9186 | total |
| bench-glm4-32b-1c-8192 | 1 | 20220 | 12571 | 7649 | total |
| bench-kimi-dev-72b-iq2s-2-2048 | 2 | 28649 | 25157 | 3492 | total |
| bench-kimi-dev-72b-iq2s-2-32768 | 2 | 38729 | 25157 | 13572 | total |
| bench-kimi-dev-72b-iq2s-2-8192 | 2 | 30665 | 25157 | 5508 | total |
| bench-kimi-dev-72b-iq2s-3-32768 | 3 | 38729 | 37728 | 1001 | total |
| bench-llama33-70b-q2-1a-2048 | 1 | 27084 | 12598 | 14486 | total |
| bench-llama33-70b-q2-1a-32768 | 1 | 37164 | 12598 | 24566 | total |
| bench-llama33-70b-q2-1a-8192 | 1 | 29100 | 12598 | 16502 | total |
| bench-llama33-70b-q2-1b-2048 | 1 | 27084 | 12559 | 14525 | total |
| bench-llama33-70b-q2-1b-32768 | 1 | 37164 | 12559 | 24605 | total |
| bench-llama33-70b-q2-1b-8192 | 1 | 29100 | 12559 | 16541 | total |
| bench-llama33-70b-q2-1c-2048 | 1 | 27084 | 12571 | 14513 | total |
| bench-llama33-70b-q2-1c-32768 | 1 | 37164 | 12571 | 24593 | total |
| bench-llama33-70b-q2-1c-8192 | 1 | 29100 | 12571 | 16529 | total |
| bench-llama33-70b-q2-2-2048 | 2 | 27084 | 25157 | 1927 | total |
| bench-llama33-70b-q2-2-32768 | 2 | 37164 | 25157 | 12007 | total |
| bench-llama33-70b-q2-2-8192 | 2 | 29100 | 25157 | 3943 | total |
| bench-llama33-70b-q2-3-32768 | 3 | 37164 | 37728 | 65 | one share |
| bench-mixtral-8x7b-1a-2048 | 1 | 26747 | 12598 | 14149 | total |
| bench-mixtral-8x7b-1a-32768 | 1 | 30779 | 12598 | 18181 | total |
| bench-mixtral-8x7b-1a-8192 | 1 | 27553 | 12598 | 14955 | total |
| bench-mixtral-8x7b-1b-2048 | 1 | 26747 | 12559 | 14188 | total |
| bench-mixtral-8x7b-1b-32768 | 1 | 30779 | 12559 | 18220 | total |
| bench-mixtral-8x7b-1b-8192 | 1 | 27553 | 12559 | 14994 | total |
| bench-mixtral-8x7b-1c-2048 | 1 | 26747 | 12571 | 14176 | total |
| bench-mixtral-8x7b-1c-32768 | 1 | 30779 | 12571 | 18208 | total |
| bench-mixtral-8x7b-1c-8192 | 1 | 27553 | 12571 | 14982 | total |
| bench-mixtral-8x7b-2-2048 | 2 | 26747 | 25157 | 1590 | total |
| bench-mixtral-8x7b-2-32768 | 2 | 30779 | 25157 | 5622 | total |
| bench-mixtral-8x7b-2-8192 | 2 | 27553 | 25157 | 2396 | total |
| bench-qwen25-72b-q2-1a-2048 | 1 | 30525 | 12598 | 17927 | total |
| bench-qwen25-72b-q2-1a-32768 | 1 | 40605 | 12598 | 28007 | total |
| bench-qwen25-72b-q2-1a-8192 | 1 | 32541 | 12598 | 19943 | total |
| bench-qwen25-72b-q2-1b-2048 | 1 | 30525 | 12559 | 17966 | total |
| bench-qwen25-72b-q2-1b-32768 | 1 | 40605 | 12559 | 28046 | total |
| bench-qwen25-72b-q2-1b-8192 | 1 | 32541 | 12559 | 19982 | total |
| bench-qwen25-72b-q2-1c-2048 | 1 | 30525 | 12571 | 17954 | total |
| bench-qwen25-72b-q2-1c-32768 | 1 | 40605 | 12571 | 28034 | total |
| bench-qwen25-72b-q2-1c-8192 | 1 | 32541 | 12571 | 19970 | total |
| bench-qwen25-72b-q2-2-2048 | 2 | 30525 | 25157 | 5368 | total |
| bench-qwen25-72b-q2-2-32768 | 2 | 40605 | 25157 | 15448 | total |
| bench-qwen25-72b-q2-2-8192 | 2 | 32541 | 25157 | 7384 | total |
| bench-qwen25-72b-q2-3-32768 | 3 | 40605 | 37728 | 2877 | total |
| bench-qwen27b-1a-2048 | 1 | 18013 | 12598 | 5415 | total |
| bench-qwen27b-1a-32768 | 1 | 26203 | 12598 | 13605 | total |
| bench-qwen27b-1a-8192 | 1 | 19651 | 12598 | 7053 | total |
| bench-qwen27b-1b-2048 | 1 | 18013 | 12559 | 5454 | total |
| bench-qwen27b-1b-32768 | 1 | 26203 | 12559 | 13644 | total |
| bench-qwen27b-1b-8192 | 1 | 19651 | 12559 | 7092 | total |
| bench-qwen27b-1c-2048 | 1 | 18013 | 12571 | 5442 | total |
| bench-qwen27b-1c-32768 | 1 | 26203 | 12571 | 13632 | total |
| bench-qwen27b-1c-8192 | 1 | 19651 | 12571 | 7080 | total |
| bench-qwen27b-2-32768 | 2 | 26203 | 25157 | 1046 | total |
| bench-qwen3-coder-30b-1a-2048 | 1 | 18784 | 12598 | 6186 | total |
| bench-qwen3-coder-30b-1a-32768 | 1 | 21808 | 12598 | 9210 | total |
| bench-qwen3-coder-30b-1a-8192 | 1 | 19389 | 12598 | 6791 | total |
| bench-qwen3-coder-30b-1b-2048 | 1 | 18784 | 12559 | 6225 | total |
| bench-qwen3-coder-30b-1b-32768 | 1 | 21808 | 12559 | 9249 | total |
| bench-qwen3-coder-30b-1b-8192 | 1 | 19389 | 12559 | 6830 | total |
| bench-qwen3-coder-30b-1c-2048 | 1 | 18784 | 12571 | 6213 | total |
| bench-qwen3-coder-30b-1c-32768 | 1 | 21808 | 12571 | 9237 | total |
| bench-qwen3-coder-30b-1c-8192 | 1 | 19389 | 12571 | 6818 | total |
| bench-qwen30b-a3b-1a-2048 | 1 | 18860 | 12598 | 6262 | total |
| bench-qwen30b-a3b-1a-32768 | 1 | 21884 | 12598 | 9286 | total |
| bench-qwen30b-a3b-1a-8192 | 1 | 19465 | 12598 | 6867 | total |
| bench-qwen30b-a3b-1b-2048 | 1 | 18860 | 12559 | 6301 | total |
| bench-qwen30b-a3b-1b-32768 | 1 | 21884 | 12559 | 9325 | total |
| bench-qwen30b-a3b-1b-8192 | 1 | 19465 | 12559 | 6906 | total |
| bench-qwen30b-a3b-1c-2048 | 1 | 18860 | 12571 | 6289 | total |
| bench-qwen30b-a3b-1c-32768 | 1 | 21884 | 12571 | 9313 | total |
| bench-qwen30b-a3b-1c-8192 | 1 | 19465 | 12571 | 6894 | total |
| m-gemma-8k-1ps5 | 1 | 12642 | 12598 | 44 | total |
| m-gemma-96k | 2 | 42260 | 25157 | 17103 | total |
