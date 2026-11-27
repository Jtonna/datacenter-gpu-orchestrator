// Package llamaflags reads the two model-server flags a profile's
// extra_args may carry that bound the host process's own memory: the
// restore points (context checkpoints) per slot and the prompt cache.
// Potluck interprets them: the launch passes the profile's value in place of
// the cluster's (never both), and the scheduler counts the maximum they
// allow on the machine that hosts the model.
//
// Spellings, at the pinned server source (llama.cpp a01665d30de2):
//
//	common/arg.cpp:1681  -ctxcp, --ctx-checkpoints, --swa-checkpoints  N
//	common/arg.cpp:1699  -cram, --cache-ram  N  (MiB; -1 no limit, 0 off)
//
// A long flag's underscores read as dashes (arg.cpp:838-840), so
// --ctx_checkpoints is the same flag. There is no --flag=value form: the
// server takes the next argument as the value (arg.cpp:869) and refuses an
// argument it does not know (arg.cpp:842), so "--cache-ram=0" stops it.
// A repeated flag only warns there and the last one wins (arg.cpp:844-849);
// here it is refused. The environment forms (LLAMA_ARG_CTX_CHECKPOINTS,
// LLAMA_ARG_CACHE_RAM) are not read.
package llamaflags

import (
	"fmt"
	"strconv"
	"strings"
)

// Which flag a spelling is.
const (
	CtxCheckpoints = "--ctx-checkpoints"
	CacheRAM       = "--cache-ram"
)

// MaxCtxCheckpoints bounds the restore points per slot, as the setting
// ctx_checkpoints is bounded.
const MaxCtxCheckpoints = 1024

var spellings = map[string]string{
	"-ctxcp": CtxCheckpoints, "--ctx-checkpoints": CtxCheckpoints, "--swa-checkpoints": CtxCheckpoints,
	"-cram": CacheRAM, "--cache-ram": CacheRAM,
}

// Spellings lists every spelling of flag (CtxCheckpoints or CacheRAM).
func Spellings(flag string) []string {
	var out []string
	for _, s := range []string{"-ctxcp", "--ctx-checkpoints", "--swa-checkpoints", "-cram", "--cache-ram"} {
		if spellings[s] == flag {
			out = append(out, s)
		}
	}
	return out
}

// flagOf is the flag arg spells ("" when none), and whether it was written
// with "=value".
func flagOf(arg string) (flag string, eq bool) {
	name, _, eq := strings.Cut(arg, "=")
	if strings.HasPrefix(name, "--") {
		name = strings.ReplaceAll(name, "_", "-")
	}
	return spellings[name], eq
}

// HostMemory is what a profile's extra_args say: the restore points per
// slot and the prompt cache in MiB, each with Has* set when the profile
// says it (and As: the spelling it used).
type HostMemory struct {
	CtxCheckpoints    int
	HasCtxCheckpoints bool
	CtxAs             string
	CacheRAMMiB       int
	HasCacheRAM       bool
	CacheAs           string
}

// Parse reads args. problems are plain sentences, one per fault (a value
// missing, not a whole number, negative or too large; a flag given twice;
// the "=" form); a flag with a problem is not taken.
func Parse(args []string) (h HostMemory, problems []string) {
	seen := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		flag, eq := flagOf(a)
		if flag == "" {
			continue
		}
		name, _, _ := strings.Cut(a, "=")
		if eq {
			problems = append(problems, fmt.Sprintf("%q: the model server does not read %s=value and would not start; write %s and its value as two entries", a, name, name))
			continue
		}
		if i+1 >= len(args) {
			problems = append(problems, fmt.Sprintf("%s has no value after it: give a whole number as the next entry", a))
			continue
		}
		raw := args[i+1]
		i++
		if first, dup := seen[flag]; dup {
			problems = append(problems, fmt.Sprintf("%s is given twice (%s and %s); keep one", flag, first, a))
			if flag == CtxCheckpoints {
				h.CtxCheckpoints, h.HasCtxCheckpoints, h.CtxAs = 0, false, ""
			} else {
				h.CacheRAMMiB, h.HasCacheRAM, h.CacheAs = 0, false, ""
			}
			continue
		}
		seen[flag] = a
		v, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s %s: the value must be a whole number", a, raw))
			continue
		case v < 0:
			problems = append(problems, fmt.Sprintf("%s %s: the value must not be negative", a, raw))
			continue
		case flag == CtxCheckpoints && v > MaxCtxCheckpoints:
			problems = append(problems, fmt.Sprintf("%s %s: at most %d restore points per slot", a, raw, MaxCtxCheckpoints))
			continue
		}
		if flag == CtxCheckpoints {
			h.CtxCheckpoints, h.HasCtxCheckpoints, h.CtxAs = v, true, a
		} else {
			h.CacheRAMMiB, h.HasCacheRAM, h.CacheAs = v, true, a
		}
	}
	return h, problems
}

// Strip is args without the two flags and their values, in every spelling
// (the "=" form too): the launch passes the effective values itself, once.
func Strip(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if flag, eq := flagOf(a); flag != "" {
				if !eq && i+1 < len(args) {
					if _, err := strconv.Atoi(args[i+1]); err == nil || !strings.HasPrefix(args[i+1], "-") {
						i++ // its value
					}
				}
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// Has reports whether args carry either flag in any spelling.
func Has(args []string) bool { return len(Strip(args)) != len(args) }
