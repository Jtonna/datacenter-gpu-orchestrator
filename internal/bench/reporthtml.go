package bench

import (
	"bytes"
	"fmt"
	"html/template"
	"net/url"
	"sort"
	"strings"
)

// ReportHTML renders a report as a page of its own with a print layout:
// "Download as PDF" is the browser's own print to PDF, "Download as
// Markdown" the same report as text. mdLink is the Markdown address.
func ReportHTML(rep *Report, mdLink string) ([]byte, error) {
	var b bytes.Buffer
	err := reportTpl.Execute(&b, map[string]any{"R": rep, "MD": mdLink, "Title": reportTitle(rep)})
	return b.Bytes(), err
}

func reportTitle(rep *Report) string {
	switch rep.Kind {
	case ReportSummary:
		return "Summary: " + rep.Run.Name
	case ReportBenchmark:
		return rep.Benchmark.Name
	case ReportSection:
		return sectionTitle(rep.Section)
	}
	return "Report"
}

// htmlPath turns an API link of a report or transcript into its HTML form.
func htmlPath(p string) string {
	if !strings.Contains(p, "/report") {
		return p
	}
	u, err := url.Parse(p)
	if err != nil {
		return p
	}
	q := u.Query()
	q.Set("format", "html")
	u.RawQuery = q.Encode()
	return u.String()
}

var reportTpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"num":   fmtNum,
	"val":   fmtVal,
	"time":  fmtTime,
	"html":  htmlPath,
	"plus1": func(i int) int { return i + 1 },
	"stitle": func(s *SectionReport) string {
		return sectionTitle(s)
	},
	"conds": func(m map[string]string) string {
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+": "+m[k])
		}
		return strings.Join(parts, "; ")
	},
	"place": func(in any) string {
		if p, ok := in.(interface{ String() string }); ok {
			return p.String()
		}
		return fmt.Sprint(in)
	},
	"onoff": func(b *bool) string {
		if b == nil {
			return ""
		}
		if *b {
			return "on"
		}
		return "off"
	},
	"deref": func(b *bool) bool { return b != nil && *b },
	"sec":   func(s SectionReport) *SectionReport { return &s },
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{--fg:#1b1f24;--mut:#5b6573;--line:#d8dde3;--bg:#fff;--inset:#f4f6f8;--err:#a1261b}
@media (prefers-color-scheme:dark){:root{--fg:#e6e9ed;--mut:#9aa4b1;--line:#333a44;--bg:#14171b;--inset:#1c2026;--err:#ff8a7d}}
body{font:14px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;color:var(--fg);background:var(--bg);margin:0 auto;max-width:1100px;padding:24px 16px}
h1{font-size:22px;margin:0 0 12px}h2{font-size:17px;margin:28px 0 8px;border-bottom:1px solid var(--line);padding-bottom:4px}h3{font-size:15px;margin:18px 0 6px}
table{border-collapse:collapse;width:100%;margin:6px 0 12px;font-variant-numeric:tabular-nums}th,td{border:1px solid var(--line);padding:4px 8px;text-align:left;vertical-align:top}
th{background:var(--inset);font-weight:600}td.n{text-align:right;white-space:nowrap}.mut{color:var(--mut)}.issue{color:var(--err)}code{font-size:13px}
.bar{display:flex;gap:8px;justify-content:flex-end;margin-bottom:12px}.bar a,.bar button{font:inherit;padding:6px 12px;border:1px solid var(--line);border-radius:8px;background:var(--inset);color:var(--fg);text-decoration:none;cursor:pointer}
.wrap{overflow-x:auto}
@media print{.bar{display:none}body{max-width:none;padding:0;font-size:11px}:root{--fg:#000;--mut:#444;--line:#999;--bg:#fff;--inset:#eee;--err:#000}a{color:inherit;text-decoration:none}h2{break-after:avoid}table{break-inside:auto}tr{break-inside:avoid}}
</style></head><body>
<div class="bar"><a href="{{.MD}}" download>Download as Markdown</a><button type="button" onclick="window.print()">Download as PDF</button></div>
{{$r := .R}}
<h1>{{.Title}}</h1>
{{define "run"}}<div class="wrap"><table>
<tr><th>Run</th><td><code>{{.ID}}</code> ({{.Kind}})</td></tr>
<tr><th>State</th><td>{{.State}}{{if .Error}}: {{.Error}}{{end}}</td></tr>
<tr><th>Started</th><td>{{time .StartedAt}}</td></tr><tr><th>Finished</th><td>{{time .FinishedAt}}</td></tr>
{{if .Release}}<tr><th>Release</th><td>{{.Release}}</td></tr>{{end}}
{{if .ConfigRevision}}<tr><th>Configuration revision</th><td>{{.ConfigRevision}}</td></tr>{{end}}
{{if .Note}}<tr><th>Note</th><td>{{.Note}}</td></tr>{{end}}
</table></div>{{end}}
{{define "issues"}}{{if .}}<ul>{{range .}}<li class="issue">{{.Text}}</li>{{end}}</ul>{{else}}<p class="mut">None.</p>{{end}}{{end}}
{{define "figures"}}<div class="wrap"><table><tr><th>Figure</th><th>Of</th><th>Machine</th><th>n</th><th>Value</th><th>Lowest</th><th>Highest</th></tr>
{{range .}}<tr><td>{{.Metric}}</td><td>{{.Subject}}</td><td>{{.Node}}</td><td class="n">{{.N}}</td><td class="n">{{val .Value .Of .Unit}}</td><td class="n">{{num .Min}}</td><td class="n">{{num .Max}}</td></tr>{{end}}
</table></div>{{end}}
{{define "section"}}<h2 id="phase-{{.Idx}}">{{stitle .}}</h2>
<p>{{.State}}, {{num .DurationS}} s.{{if .Transcript}} <a href="{{.Transcript}}">Transcript</a>.{{end}}</p>
{{if .Conditions}}<p class="mut">Conditions: {{conds .Conditions}}.</p>{{end}}
{{if .Figures}}{{template "figures" .Figures}}{{end}}
{{range .Issues}}<p class="issue">Issue: {{.Text}}</p>{{end}}{{end}}

{{if eq $r.Kind "summary"}}
{{template "run" $r.Run}}
<h2>Headline</h2>
{{with $r.Headline}}<p>{{.Steps}} steps. Ran {{.Ran}}: {{.Completed}} completed, {{.Failed}} failed, {{.Refused}} refused{{if .Cancelled}}, {{.Cancelled}} cancelled{{end}}{{if .Skipped}}; {{.Skipped}} skipped{{end}}.</p>{{end}}
<h2>Issues</h2>{{template "issues" $r.Issues}}
{{if $r.Tables}}<h2>Measurements</h2>
{{range $r.Tables}}<h3>{{.Metric}} ({{.Unit}})</h3>{{if .Description}}<p class="mut">{{.Description}}</p>{{end}}
<div class="wrap"><table><tr><th>Benchmark</th><th>Phase</th><th>Model</th><th>Context</th><th>Placement</th><th>n</th><th>Average</th><th>Lowest</th><th>Highest</th></tr>
{{range .Rows}}<tr><td>{{.Name}}</td><td>{{.Phase}}</td><td>{{.Model}}</td><td class="n">{{if .Ctx}}{{.Ctx}}{{end}}</td><td>{{.Placement}}</td><td class="n">{{.N}}</td><td class="n">{{val .Average .Of ""}}</td><td class="n">{{num .Lowest}}</td><td class="n">{{num .Highest}}</td></tr>{{end}}
</table></div>{{end}}{{end}}
<h2>Benchmark reports</h2>
<div class="wrap"><table><tr><th>#</th><th>Benchmark</th><th>Profile</th><th>Outcome</th></tr>
{{range $r.Index}}<tr><td class="n">{{plus1 .Step}}</td><td><a href="{{html .Report}}">{{.Name}}</a></td><td><code>{{.Profile}}</code></td><td>{{.Outcome}}{{if and .Reason (ne .Outcome "ready")}}: {{.Reason}}{{end}}</td></tr>{{end}}
</table></div>
{{else if eq $r.Kind "benchmark"}}{{with $r.Benchmark}}
{{template "run" $r.Run}}
<h2>Inputs</h2><div class="wrap"><table>
{{if .Recipe}}<tr><th>Recipe</th><td><code>{{.Recipe}}</code></td></tr>{{end}}
<tr><th>Benchmark</th><td><code>{{.BenchmarkID}}</code></td></tr><tr><th>Profile</th><td><code>{{.Profile}}</code></td></tr>
{{if .Model}}<tr><th>Model</th><td>{{.Model}}</td></tr>{{end}}
{{with .Inputs}}{{if .Ctx}}<tr><th>Context</th><td>{{.Ctx}}</td></tr>{{end}}<tr><th>Placement</th><td>{{place .Place}}</td></tr>
{{if .Thinking}}<tr><th>Thinking</th><td>{{onoff .Thinking}}</td></tr>{{end}}{{if .KVQuant}}<tr><th>KV cache</th><td>{{.KVQuant}}</td></tr>{{end}}{{end}}
<tr><th>Outcome</th><td>{{.Outcome}}{{if and .Reason (ne .Outcome "ready")}}: {{.Reason}}{{end}}</td></tr>
<tr><th>Duration</th><td>{{num .DurationS}} s</td></tr>
{{if .Transcript}}<tr><th>Transcript</th><td><a href="{{.Transcript}}">transcript</a></td></tr>{{end}}
</table></div>
<h2>Issues</h2>{{template "issues" $r.Issues}}
{{if .StepFigures}}<h2>The whole step</h2>{{template "figures" .StepFigures}}{{end}}
{{range .Sections}}{{template "section" sec .}}{{end}}
{{end}}{{else if eq $r.Kind "section"}}
<p class="mut">Run <code>{{$r.Run.ID}}</code> ({{$r.Run.Name}}).</p>
{{template "section" $r.Section}}
{{end}}
</body></html>
`))
