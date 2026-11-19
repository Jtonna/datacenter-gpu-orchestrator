# UI screenshot review

Screenshots are taken with `scripts/ui-screens.sh` against the dev harness
(`scripts/ui-dev.sh`). File names: `<screen>-<scenario>-<desktop|phone>.png`
(desktop 1440 px wide, phone 390 px wide; the height is chosen to show the
whole page). Each pass looked at every picture and compared it with the intent
of the reference design ("Roost screens"): alignment, spacing, overflow,
truncated text, contrast, empty and broken states, phone layout, numbers that
line up.

## Method notes (found while reviewing)

- Desktop Chrome does not lay a window out narrower than about 500 px, so the
  first phone pictures were a 504 px layout cropped to 390 px (text cut off at
  the right edge, which looked like an overflow bug and was not). Phone
  pictures now go through the harness page `/dev/frame`, which shows the UI in
  a frame of the exact width.
- A picture taken through `--timeout` sometimes showed only the loading
  placeholders. The script now uses `--virtual-time-budget`, and the views draw
  on a microtask instead of a timer (a background tab throttles timers; a
  monitoring page must not fall behind because of that).

## Pass 1 (Monitor group, Placement preview, About)

| Screen | Found | Changed |
|---|---|---|
| Shell | Renders as designed. Mono text inside bold runs was drawn with a faked bold (only weights 400 and 500 are bundled). | `.mono` is 400, at most 500 inside headings and links. |
| Models, loading | Step tracker, overall percent, time left, deadline and per-worker rows read well. | None. |
| Models, split / twins | Memory rows line up; the model file sits at the right of the header. | None. |
| Models, phone | Command and log file took a 120 px label column; profile ids wrapped inside the scrolling table. | Fact lists stack on a phone; profile ids do not wrap; tighter card padding under 700 px. |
| Models, not loaded | "Where it would go" repeated internal strings ("placer: need 12100 MiB (requires 12100 MiB) does not fit the available nodes", "forced nodes in use: ...", "Joins the queued entry (1 waiting, claimed false)"). | `plainReason()` rewrites the known wordings in plain words ("Needs 12,100 MiB; the nodes that are free now do not have that much."); the scheduler's own text stays in the tooltip. An unknown wording is only tidied, never guessed at. |
| Nodes | The priority label wrapped to a second header line on a tile with two badges. | Priority moved to the tile footer with the address and the id. |
| Queue | Three identical lines for three nodes holding one model; durations in bold mono looked heavy; the badge column stretched; a claimed entry repeated its victim as "node is busy". | Nodes holding the same model are one line; durations are in the text face; badges align right; a victim is listed once. |
| Queue | "Placer: need ..." as the headline of a waiting request. | Headline uses `plainReason()`; raw text in the tooltip. |
| Activity | "1 h 33 min ago" wrapped in the time column; a "Show the record" link under every row was noisy; the second line repeated the first ("took 6.1 s"); the loads table was cut off in the side column. | Wider time column in the text face; a small "Record" toggle at the right of the row; the second line only adds what the sentence lacks; loads table shows seconds, the node count as a second line, and fits. |
| Placement preview | Profile ids wrapped in the list. | List is wider; ids are cut with an ellipsis and a tooltip. A refusal shows the plain reason and, under it, what the scheduler said. |
| About | "Load memory floor MiB 768 MiB"; values in mono looked like code. | The unit is dropped from the label; values are in the text face. |

## Pass 2 (same screens, after the fixes; plus empty, stale, refused, bare)

| Screen | Found | Changed |
|---|---|---|
| Models, bare (no configuration) | The "no configuration yet" banner and "No profiles yet" read well. | None. |
| Models, refused | The failed-load banner states what failed, when, and why, with a link to the events of that load. | None. |
| Models, twins | Two cards for one model file, each naming its node and the other profile. `stale_profile` shows "Older definition". | None. |
| Activity, history off | The time range and Download were offered although there is nothing to read; the text named a YAML key that is now a start flag. | The header drops the range and the download; the text names `--history-dir`. |
| Queue, phone | The "why" column of the refused table is reached by scrolling the table sideways; acceptable, the first three columns identify the row. | None. |
| Reconnect | Killing the dev server shows "Reconnecting… Last update 8 s ago" and the warning banner within 4 s; after a restart the page is live again in 2 s and re-reads the profile list. | None. |
