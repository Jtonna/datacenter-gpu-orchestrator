# Review of the Configure screens and Audit

Screens: Profiles, Tiers and nodes, Settings, Audit. Every picture is in this folder, named
`<screen>-<scenario>-<desktop|phone>.png`. Behaviour was driven in headless Chrome through the
DevTools protocol against the harness (edit, review, save, 400, 409, rollback, live prepend).

## Pass 1 (first screenshots and first flows)
Found and changed:
- Profiles: saving an edit sent the OLD value. Each section form returns the whole profile, so the
  last form overwrote the edited field. Fix: each form only contributes its own fields.
- Profiles: every field with no default showed "Changed from default" (the schema sends `default: null`).
  Fix: null defaults are not passed to the form. Placeholders now say what an empty field means.
- Profiles list: model file names wrapped on every hyphen and the action column was too wide. Edit
  button dropped (the row opens the editor); memory sub-line is sans.
- Settings: the text "null" appeared in every meta line (`replaceChildren` with null). Fix: `frag()`.
  The long probe command in "Default" is shortened.
- Review dialog: opened with no focusable element (JS error). It now has Cancel from the start; the
  revision viewer too.
- Tiers: label/value pairs ran together; "shared with the system" was mono. Placeholder cut off.
- Audit: "Show details" sat on its own line and made every row tall; it is now a link in the detail line.

## Pass 2 (after the fixes, with the flows)
- Save button said "Save 13 changes" for a tier reorder (counted diff rows). Ops now carry a count;
  a reorder says "Save 1 change" and shows the new order in words above the server diff.
- 409: the "someone else changed" banner stayed after a successful save; cleared. The notice said
  "changed multiple": wording falls back to "the configuration".
- Restart keys were shown as `settings.server_bin`; the prefix is dropped.
- Revision summary grammar ("1 excluded nodes") fixed.
- A staged new profile that the server refused was titled "Edit profile"; now "New profile".
- Empty states: not configured (all four screens, desktop and phone), no profiles (button), no audit entries.
- Phone (390 px): tables scroll inside their card, tier cards stack, filters wrap, dialogs fit.

Left as is: the audit time column wraps "1 h 10 min ago" (shared `.ev` grid is 86 px).
