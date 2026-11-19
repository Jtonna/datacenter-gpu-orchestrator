// A stand-in for a view that is not built yet. Replace the whole view file;
// do not edit this one.

import { h } from '../core/dom.js';
import { registerView } from '../core/router.js';
import { pageHead, empty } from '../core/ui.js';

export function placeholder(id, title, text) {
  registerView({
    id, title,
    mount(el) {
      el.replaceChildren(pageHead(title), h('section.card', empty('This screen is being built', text)));
      return null;
    },
  });
}
