// The picture of a model: the icon the owner set (for the model, or for its
// whole family), else an avatar drawn here: a rounded tile with a monogram, in
// a quiet hue that belongs to the model family (the letters come from the
// model's own name, see monogram). These hues are identity, not
// state: they are muted on purpose and appear nowhere else in the UI (the
// state colours mint, amber, coral and lilac are never used for an avatar).
//
// A model comes from the catalog (GET /api/models): `brand` names the family,
// `icon` is {etag, scope} when an icon is stored, `key` addresses it.

import { h } from './dom.js';
import { url } from './api.js';

// family -> hue. The hue says the family; the letters say the model.
const BRANDS = {
  gemma: 214, qwen: 262, llama: 196, mistral: 24, deepseek: 232, glm: 174,
  kimi: 318, tinyllama: 88, phi: 286, granite: 150,
};

// firstWord(m) -> the first word of the model's own name: short_name, else
// name, else key, split at anything that is not a letter or a digit
// ("devstral-small-2505" -> "devstral", "qwen3-14b" -> "qwen3").
const firstWord = m => String(m.short_name || m.name || m.key || '').split(/[^A-Za-z0-9]+/).filter(Boolean)[0] || '';

// monogram(m) -> the first two letters of that word, "De" for devstral,
// "Mi" for mixtral, "Qw" for qwen3; "?" when there is no name. It comes from
// the model, never from its publisher (two models of one publisher would
// share it).
export function monogram(m) {
  const w = firstWord(m || {});
  if (!w) return '?';
  return w.charAt(0).toUpperCase() + w.charAt(1).toLowerCase();
}

// hueOf(m) -> the family's hue, or for a family the server does not know a
// hue from the first word, so two unknown models with the same letters
// rarely share a colour. null when there is no name at all.
export function hueOf(m) {
  if (BRANDS[m.brand] != null) return BRANDS[m.brand];
  const w = firstWord(m).toLowerCase();
  if (!w) return null;
  let x = 0;
  for (const c of w) x = (x * 31 + c.charCodeAt(0)) % 997;
  return 15 + 30 * (x % 12); // twelve hues 30 degrees apart: two of them never look alike
}

// avatar(model, 28 | 40 | 72) -> the tile. Pass null for a model that is not
// in the catalog (a neutral tile).
export function avatar(m, size = 28, attrs = {}) {
  m = m || {};
  const cls = 'av av-' + size;
  if (m.icon && m.icon.etag && m.key) {
    return h('span', Object.assign({ class: cls + ' img', 'aria-hidden': 'true' }, attrs),
      h('img', { src: url('/api/models/' + encodeURIComponent(m.key) + '/icon', { v: m.icon.etag }), alt: '', width: size, height: size }));
  }
  if (m.kind === 'command' || m.brand === 'command') {
    // a program on this machine, not a model file: a neutral tile with a prompt glyph
    const s = Math.round(size * 0.52);
    return h('span', Object.assign({ class: cls + ' av-cmd', 'aria-hidden': 'true' }, attrs),
      h('svg', { viewBox: '0 0 20 20', width: s, height: s },
        h('path', { d: 'M4 6l4 4-4 4M10.5 14.5H16', fill: 'none', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round' })));
  }
  const hue = hueOf(m);
  return h('span', Object.assign({ class: cls + (hue == null ? ' plain' : ''), style: hue == null ? null : `--hue:${hue}`, 'aria-hidden': 'true', 'data-mono': monogram(m) }, attrs), monogram(m));
}

// named(model, node, size) -> the avatar and a name side by side (a table cell, a card header).
export const named = (m, node, size = 28) => h('span.named', avatar(m, size), h('span.grow', node));
