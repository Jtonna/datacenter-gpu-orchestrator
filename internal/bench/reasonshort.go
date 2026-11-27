package bench

import (
	"fmt"
	"strings"
)

// ShortReason is a refused step's reason in a few words, for a list row
// (board Bench-Runs-Group: "Does not fit on 1 zone"); the full sentence
// stays in Reason. Computed when a step is read; nothing stored. Empty for a
// step that was not refused (a failure keeps its own sentence) or has no
// reason.
func ShortReason(outcome, reason string, nodeCount int) string {
	if reason == "" || (outcome != OutcomeRefused && outcome != OutcomeRefusedByExclusion) {
		return ""
	}
	low := strings.ToLower(reason)
	where := ""
	if nodeCount > 0 {
		where = " on " + zones(nodeCount)
	}
	switch {
	case strings.Contains(low, "does not fit"), strings.Contains(low, "would_not_fit"), strings.Contains(low, "would not fit"):
		return "Does not fit" + where
	// before "excluded": the sentence explains itself with "not excluded".
	// "is not a console" is the sentence of runs stored before build-92.
	case strings.Contains(low, "is not a placeable zone"), strings.Contains(low, "is not a console"):
		return "Not a placeable zone"
	case outcome == OutcomeRefusedByExclusion || strings.Contains(low, "excluded"):
		return "A zone is excluded"
	case strings.Contains(low, "can take a load now"), strings.Contains(low, "cannot take a load now"):
		return "Not enough zones free"
	case strings.Contains(low, "backoff"), strings.Contains(low, "backing off"):
		return "Backing off after a failure"
	case strings.Contains(low, "busy"), strings.Contains(low, "not idle"):
		return "The fleet was busy"
	}
	// otherwise the first clause, at most 48 characters
	s := reason
	if i := strings.IndexAny(s, ":;("); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "."))
	if r := []rune(s); len(r) > 48 {
		s = strings.TrimSpace(string(r[:47])) + "…"
	}
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func zones(n int) string {
	if n == 1 {
		return "1 zone"
	}
	return fmt.Sprintf("%d zones", n)
}
