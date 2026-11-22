package config

import "strings"

// PoolPhrases turns the fleet wording of a message into the wording of
// membership agents, in this order (the first phrase that matches at a
// position wins, so a longer phrase comes before a shorter one it contains).
// Every message is written once, in its fleet wording; PoolText derives the
// other. The web UI has the same table in core/words.js, and a test keeps the
// two equal.
var PoolPhrases = [][2]string{
	{"the fleet does not report", "is not in the pool"},
	{"The fleet reports", "Its agent reports"},
	{"the fleet reports", "its agent reports"},
	{"fleet figures", "agent figures"},
	{"the fleet view", "the pool"},
	{"the fleet feed", "the pool"},
	{"fleet view", "pool"},
	{"the fleet", "the pool"},
}

var poolReplacer = func() *strings.Replacer {
	var pairs []string
	for _, p := range PoolPhrases {
		pairs = append(pairs, p[0], p[1])
	}
	return strings.NewReplacer(pairs...)
}()

// PoolText words text for the membership: unchanged with membership fleet,
// with the fleet phrases of PoolPhrases replaced when agents is true.
func PoolText(agents bool, text string) string {
	if !agents {
		return text
	}
	return poolReplacer.Replace(text)
}

// PoolText words text for the membership of c (see the function PoolText).
func (c *Config) PoolText(text string) string { return PoolText(c.AgentsMode(), text) }
