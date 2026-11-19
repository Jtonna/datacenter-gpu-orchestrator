# Round 3: review against the owner's feedback

Pictures are of the `real` scenario (a recording of the real cluster: 16
models, 254 profiles, two benchmark runs) unless the name ends in `-demo` (the
scripted cluster, which has more runs). `-before` is the build this round
started from (b7abf1e), `-after` is this round. 1440 px wide; `-phone` is
390 px.

Three review passes were made from screenshots, and one with a script that
drives the page (hover, click, type) for what a still picture cannot show.

## What he asked, what was built

### 1. "models page ... some kind of chart to show the last few loads ... instead of a number"

`models-before.png` -> `models-after.png`, `models-after-demo.png`, `models-after-phone.png`

Every row has a "Last loads" strip: one bar per load (the newest 12), oldest
left, newest right, with the newest load time and speed still written beside
it. Mint = loaded, coral = failed or cancelled, a hollow amber stub = a request
that was refused (no load happened). Hover a bar for when, which
configuration, how many PS5s, seconds and MB/s (`model-tooltip-after.png`
shows the same tooltip on the model page).

How the chart stays honest about node counts: the bar height is the load time
on the row's own scale, and **under every bar there are one, two or three
pips: the number of PS5s that load went onto**. The Gemma row shows why: seven
tall bars with one pip (about 68 s on one PS5) and then shorter ones with
three pips (28 s on three). Nothing is averaged across node counts. Normalising
to MB/s was rejected: he asked for load time, and MB/s hides a slow link
behind a small file.

Still weak: on a phone the table scrolls sideways, so the strip is off screen
until scrolled to. With one load the strip is a single bar.

### 2. "clicking on a model ... some more charts ... before looking at specific configurations"

`model-before.png` -> `model-after.png`, `model-after-phone.png`, `model-estimate-after.png`

A card "At a glance" sits above Usage and Configurations:

- **Memory it needs** against what one, two and three PS5s give (the fit bar,
  first and full width, with the verdict in words under it).
- **Load time** and **Upload speed** of every load in the kept history, as
  points over time; the shape is the node count (circle 1, square 2, triangle
  3), the colour the outcome.
- **On 1, 2 and 3 PS5s**: load time, upload speed, memory and answer speed as
  small bars; a figure that does not exist says "not measured yet" (answer
  speed, for Gemma: no benchmark of it sends requests).
- **Activity**: loads and refused requests per hour for the last 24 hours (per
  day for 7 days when the day was quiet), and requests served.

Each chart has a title with its unit, a labelled axis from 0, a tooltip per
mark and a sentence for screen readers.

Not built: "how long it stayed loaded" as a chart. In the real data almost
every load is a benchmark that unloads after seconds, so the chart would be a
row of stubs; the figure stays in the Usage table.

Still weak: the kept history spans five hours, so the time axes are short and
the activity chart is empty on the left. Requests are counted when a model
unloads (the history has no event per request), and the chart says so.

### 3. "benchmarks ... click it and it shows me the runs and an option to open the configuration panel"

`benchmarks-before.png`, `suite-before.png` -> `benchmarks-after.png`, `suite-after.png`, `suite-after-demo.png`, `suite-config-panel-after.png`, `benchmark-after.png`

A suite and a benchmark each have their own page; a row of the overview opens
it. Suite page: name, description, the pass dots, the run history first (when,
state, result, passed, duration, against the run before, the note, a link to
the run), then duration and benchmarks passed over the runs, then what it
runs. Benchmark page: results run by run (result, outcome, suite, PS5s, load
time, upload, answer speed, memory), a trend per figure that was measured,
what it expects and whether the last run met it. Both have "Open
configuration": the existing editor in a panel from the right, in the address
(`?edit=1`), closed by its button, Escape, a click beside it or Back. Run is
still one click with the same confirmation. The live run panel is unchanged.

Still weak: with two runs (the real data) the trends are two points; the demo
pictures show six. The panel is the full editor, so it is long.

### 4. "pass rate can be like a dot graph (like modern health monitors do)"

`benchmarks-after-demo.png`, `suite-after-demo.png`, `benchmarks-aggregates-after.png`, `dots-tooltip-after.png`

One pill per run, oldest left: mint passed, amber partly passed, coral failed,
grey cancelled or no result; the percentage beside it; each pill has a tooltip
and opens its run. Used in the Benchmarks overview (suites and benchmarks), on
the suite and benchmark pages (larger), in the aggregates, and in the
Benchmarks table of a model page.

### 5. "sidebar doesn't need a live indicator"

`models-after.png` (nothing there), `connection-warning-after.png`

The "Live / Fleet feed fresh" block is gone. When the live channel is down,
still connecting after a moment, or the fleet feed is stale or disconnected, a
slim amber bar appears above the screen and goes away by itself. "Changes are
signed as ..." stays. About is unchanged and still lists the connection among
its facts.

### 6. "about tab with it is fine enough"

Left as it is.

### 7. "a dedicated MCP sidebar option ... the config to give to our ai agents with a key so its auditable"

`mcp-after.png`, `mcp-new-key-after.png`, `mcp-after-phone.png`, `audit-before.png` -> `audit-after.png`

"MCP" is in the navigation under System. The page says in one sentence that a
key is identity, not a lock; shows the server address as the browser sees it;
lists the agents (name, key prefix, added, last used, requests, a link to
their entries in Audit; rename, new key, revoke); "Add agent" shows the key
once with three blocks to copy (an `mcpServers` block, the Claude Code
command, a curl call that lists the tools); and lists what an agent can do,
read from the server (48 tools in four groups). It states that the web UI has
no load or unload control. In Audit an agent is named with an "Agent" badge
and there is a filter by agent.

Still weak: the address is what the browser sees; on the real machine that is
a `.local` name that other machines may not resolve, and the page can only
say so.

### 8. "i know models have icons / profile pics id like to use those on the models page"

`models-after.png`, `models-custom-icon-after.png`, `model-icon-dialog-after.png`, `profiles-after.png`

Every model has an avatar: a rounded tile with two letters in a quiet hue per
family (Gemma, Qwen, Llama, Mistral/Mixtral/Devstral, DeepSeek, GLM, Kimi,
TinyLlama; a prompt glyph for the command services). No company logo is in the
repository. "Change icon" on a model's page uploads a PNG, JPEG, WebP or SVG
(256 kB at most, an SVG with a script is refused) for that model or for its
whole family; `models-custom-icon-after.png` shows an icon set once for the
Gemma family. The avatar is on the Models list, the instance cards, the model
page, the benchmark lists and pages, and the Profiles groups.

Still weak: the letters are a stand-in; they look tidy but they are not the
pictures he has in mind until he uploads them.

### "its very slow ... if we used websocket or even ran a small cache"

The catalog is kept in memory on the server, sent over the WebSocket, and kept
on the page. Measured on the `real` recording, on this machine:

| | before (b7abf1e) | after |
|---|---|---|
| `GET /api/models`, warm | 3.4 to 5.6 ms (built per request) | 1.1 to 1.7 ms (from memory) |
| `GET /api/models`, first call after a restart | 5.6 ms | 5.5 ms (built in the background at start; file metadata from the database) |
| Page: shell on screen | waited for the first status | 47 to 51 ms |
| Page: catalog rows on screen | after status, then a REST call | 82 to 86 ms (from the socket) |
| Back to Models from another screen | a REST call | 11 to 17 ms (from memory, no request) |

What made the first open slow on the real system was not this endpoint: it
answers in 30 to 60 ms there. `curl` from this machine shows 2.7 s spent
resolving the name `omen-runner.local` on the first request and 0.02 s
afterwards. The page used to show "Connecting" for that long because it drew
nothing until the socket had delivered a status. It now draws at once and
asks for the catalog over REST and the socket at the same time.

Also in this round: one rule for display names ("Mixtral 8x7B Instruct v0.1",
publisher "Mistral AI" apart), and "Runs on" says "2 PS5s (estimate) ·
measured on 3" when the measured need fits on fewer machines than it was run
on (`model-estimate-after.png`).

## Checked with a script, not only by eye

Hover tooltips on the load strip, a chart point and a pass dot; the icon
dialog; adding an agent, the key shown once and gone after "Done"; revoking;
the configuration panel opening and closing with Escape; no script error on
any of these pages.

## Not verified

- Nothing was run against the real cluster: the new endpoints and the
  migration were tested on test databases and the recording only.
- A real MCP client (Claude Code) was not connected with a key; the tests call
  `/mcp` directly with the headers.
- Uploading an icon through the file picker in a real browser (the endpoint
  and the dialog were tested separately).
