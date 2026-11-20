# Review of the Benchmarks screens

> The screenshots named on this page were removed from the repository; they can be regenerated with `go run ./cmd/devcluster`.

Screenshots are in this folder (`benchmarks-<scenario>-<desktop|phone>.png`), taken with
`scripts/ui-screens.sh` against `cmd/devcluster` on 127.0.0.1:8792. Behaviour was also driven in a
headless Chrome through the DevTools protocol (a throwaway script, not committed).

## Pass 1 (first build, old mock shapes, then the aligned mock)

Found and changed:
- The run indicator never showed in the shell. `core/shell.js` wraps the slot node in a fresh
  `data-keep` wrapper on every draw, which steals the node from the wrapper that stays. Worked
  around in `live.js` (`rehome()` puts the node back in the same task). Lead: the shell should not
  re-wrap an existing slot node.
- The editor printed the word "null": `replaceChildren(null)`. Nulls are filtered now.
- "Time limit" had two labels (field and schemaForm). One now.
- Summary read "a 3 turns conversation"; now "a 3-turn conversation".
- Phase header repeated the type as a badge next to its title; removed.
- Expectation labels were machine names ("At most: ttft ms"); a plain label per key now.
- Trend chart text was huge (the shared chart scales its viewBox to the card) and the y labels with
  units were clipped. The chart is limited to 760 px wide and the unit moved to the card note.
- Phase label showed the raw "load"; shows the schema title unless the label differs from the type.
- Scheduler reasons (refusal, paused, load reason) go through `plainReason`; raw text in the tooltip.

## Pass 2 (aligned mock, all screens, phone)

Checked every route at 1440 and 390 px: no horizontal page scroll (scrollWidth equals width on
overview, runs, run detail, aggregates, both editors); tables scroll inside their cards. Checked the
confirm dialog (loads and unloads models, benchmarks, profiles, nodes, not idle warning, note
field), cancel confirm, create benchmark from a template, add a phase, save, create a suite, run it,
watch progress, cancel, open a finished run, failed run, refused step, regression, empty states and a
long queue. Server validation errors come back next to the phase (checked with max_tokens 999999).

Left as is:
- On a phone the shell pane is a drawer, so the run indicator is only visible after opening the
  menu. A slot in the top bar would fix it (core change, for the lead).
- Metric tables of different benchmarks size their columns on their own, so columns do not line up
  from card to card.
