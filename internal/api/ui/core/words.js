// The wording of the two memberships. A text that both modes show is written
// once, in its fleet wording; with membership agents poolText() swaps the
// fleet phrases for pool phrases. The table is the same as
// PoolPhrases in internal/config/wording.go (a Go test compares the two), in
// the same order: at each position the first phrase that matches wins.
// No DOM and no import.

export const POOL_PHRASES = [
  ['the fleet does not report', 'is not in the pool'],
  ['The fleet reports', 'Its agent reports'],
  ['the fleet reports', 'its agent reports'],
  ['fleet figures', 'agent figures'],
  ['the fleet view', 'the pool'],
  ['the fleet feed', 'the pool'],
  ['fleet view', 'pool'],
  ['the fleet', 'the pool'],
];

const escape = s => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const PHRASE = new RegExp(POOL_PHRASES.map(p => escape(p[0])).join('|'), 'g');
const TO = new Map(POOL_PHRASES);

// poolText(agents, text): text as it is when agents is false, with the fleet
// phrases replaced when it is true.
export const poolText = (agents, text) => (agents ? String(text).replace(PHRASE, m => TO.get(m)) : text);
