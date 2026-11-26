package runner

import "strings"

// scrubLlamaArgs removes every LLAMA_ARG_* variable, compared
// case-insensitively: the model server reads them as arguments, and they
// would silently override the command line the scheduler built
// (--ctx-checkpoints, --cache-ram and the rest). The scheduler starts its
// own children the same way (internal/instance scrubEnv).
func scrubLlamaArgs(env []string) []string {
	const pre = "LLAMA_ARG_"
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if len(k) >= len(pre) && strings.EqualFold(k[:len(pre)], pre) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
