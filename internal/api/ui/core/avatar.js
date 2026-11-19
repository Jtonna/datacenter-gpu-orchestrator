// The picture of a model: the icon the owner set (for the model, or for its
// whole family), else an avatar drawn here: a rounded tile with a monogram, in
// a quiet hue that belongs to the model family. These hues are identity, not
// state: they are muted on purpose and appear nowhere else in the UI (the
// state colours mint, amber, coral and lilac are never used for an avatar).
//
// A model comes from the catalog (GET /api/models): `brand` names the family,
// `icon` is {etag, scope} when an icon is stored, `key` addresses it.

import { h } from './dom.js';
import { url } from './api.js';

// family -> [monogram, hue]. A family the server does not know is "other".
const BRANDS = {
  gemma: ['Ge', 214], qwen: ['Qw', 262], llama: ['Ll', 196], mistral: ['Mi', 24], deepseek: ['De', 232], glm: ['GL', 174],
  kimi: ['Ki', 318], tinyllama: ['Tl', 88], phi: ['Ph', 286], granite: ['Gr', 150],
};

function monogram(m) {
  const b = BRANDS[m.brand];
  if (b) return b[0];
  const w = String(m.name || m.key || '?').replace(/[^A-Za-z0-9 ]/g, ' ').trim().split(/\s+/)[0] || '?';
  return w.charAt(0).toUpperCase() + w.charAt(1).toLowerCase();
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
    return h('span', Object.assign({ class: cls + ' cmd', 'aria-hidden': 'true' }, attrs),
      h('svg', { viewBox: '0 0 20 20', width: s, height: s },
        h('path', { d: 'M4 6l4 4-4 4M10.5 14.5H16', fill: 'none', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round' })));
  }
  const b = BRANDS[m.brand];
  return h('span', Object.assign({ class: cls + (b ? '' : ' plain'), style: b ? `--hue:${b[1]}` : null, 'aria-hidden': 'true' }, attrs), monogram(m));
}

// named(model, node, size) -> the avatar and a name side by side (a table cell, a card header).
export const named = (m, node, size = 28) => h('span.named', avatar(m, size), h('span.grow', node));
