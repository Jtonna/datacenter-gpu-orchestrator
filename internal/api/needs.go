package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The learned memory needs as stored: one row per shape of load and device
// count (GET /api/needs), and forgetting every row of a shape (POST
// /api/needs/forget), for a measurement that should no longer count (a
// replaced model file of the same size, changed hardware).

type needRow struct {
	Shape      string    `json:"shape"`
	Kind       string    `json:"kind"`
	Model      string    `json:"model"`
	Ctx        int       `json:"ctx"`
	Parallel   int       `json:"parallel"`
	KVQuant    string    `json:"kv_quant"`
	FlashAttn  bool      `json:"flash_attn"`
	ExtraArgs  []string  `json:"extra_args"`
	Devices    int       `json:"devices"`
	TotalMiB   int       `json:"total_mib"`
	Placement  []string  `json:"placement"`
	MeasuredBy string    `json:"measured_by"`
	MeasuredAt time.Time `json:"measured_at"`
	Release    string    `json:"release"`
	LlamaBuild string    `json:"llama_build"`
	Restored   bool      `json:"restored"`
	Trusted    bool      `json:"trusted"`
	// UntrustedReason says why a row is not trusted ("" when it is).
	UntrustedReason string   `json:"untrusted_reason"`
	Profiles        []string `json:"profiles"`
}

func needRowJSON(r scheduler.LearnedRow) needRow {
	out := needRow{Shape: r.Shape, Kind: string(r.Kind), Model: r.Model, Ctx: r.Ctx, Parallel: r.Parallel, KVQuant: r.KVQuant,
		FlashAttn: r.FlashAttn, ExtraArgs: r.ExtraArgs, Devices: r.Devices, TotalMiB: r.TotalMiB, Placement: []string{},
		MeasuredBy: r.MeasuredBy, MeasuredAt: r.MeasuredAt, Release: r.Release, LlamaBuild: r.LlamaBuild, Restored: r.Restored,
		Trusted: r.Trusted, UntrustedReason: r.UntrustedWhy, Profiles: r.Profiles}
	if out.ExtraArgs == nil {
		out.ExtraArgs = []string{}
	}
	if out.Profiles == nil {
		out.Profiles = []string{}
	}
	for _, id := range r.Placement {
		out.Placement = append(out.Placement, string(id))
	}
	return out
}

// hostOverheadRow is one measured host overhead (scheduler hostoverhead.go).
type hostOverheadRow struct {
	Shape      string     `json:"shape"`
	Node       string     `json:"node"`
	NodeName   string     `json:"node_name"`
	MiB        int        `json:"mib"`
	LogHostMiB int        `json:"log_host_mib"`
	MeasuredBy string     `json:"measured_by"`
	MeasuredAt *time.Time `json:"measured_at"`
	Release    string     `json:"release"`
	LlamaBuild string     `json:"llama_build"`
	Cause      string     `json:"cause"`
	Restored   bool       `json:"restored"`
	Used       bool       `json:"used"`
	Profiles   []string   `json:"profiles"`
}

func hostOverheadJSON(r scheduler.HostOverheadRow) hostOverheadRow {
	out := hostOverheadRow{Shape: r.Shape, Node: string(r.Node), NodeName: r.NodeName, MiB: r.MiB, LogHostMiB: r.LogHostMiB,
		MeasuredBy: r.MeasuredBy, Release: r.Release, LlamaBuild: r.LlamaBuild, Cause: r.Cause, Restored: r.Restored, Used: r.Used,
		Profiles: r.Profiles}
	if !r.MeasuredAt.IsZero() {
		at := r.MeasuredAt.UTC()
		out.MeasuredAt = &at
	}
	if out.Profiles == nil {
		out.Profiles = []string{}
	}
	return out
}

func (s *Server) handleNeeds(w http.ResponseWriter, r *http.Request) {
	doc, err := s.sched.Needs(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]needRow, 0, len(doc.Rows))
	for _, row := range doc.Rows {
		out = append(out, needRowJSON(row))
	}
	// the two llama builds trust depends on ("" while not known)
	// the check of derived needs (scheduler derive.go): additive
	pairs := make([]map[string]any, 0, len(doc.DerivePairs))
	for _, p := range doc.DerivePairs {
		pairs = append(pairs, map[string]any{"model": p.Model, "shape": p.Shape, "devices": p.Devices, "from_ctx": p.FromCtx,
			"to_ctx": p.ToCtx, "from_mib": p.FromMiB, "predicted_mib": p.PredictedMiB, "measured_mib": p.MeasuredMiB,
			"error_mib": p.ErrMiB, "within_tolerance": p.WithinTolerance, "below_estimate": p.BelowEstimate})
	}
	hosts := make([]hostOverheadRow, 0, len(doc.HostOverheads))
	for _, h := range doc.HostOverheads {
		hosts = append(hosts, hostOverheadJSON(h))
	}
	writeJSON(w, http.StatusOK, map[string]any{"needs": out, "host_build": doc.HostBuild, "release": doc.Release,
		"served_build": doc.ServedBuild, "host_overheads": hosts, "host_overhead_setting_mib": doc.HostOverheadSettingMiB,
		"host_overhead_setting_only": doc.HostOverheadSettingOnly,
		"derivation_check":           map[string]any{"tolerance_mib": doc.DeriveToleranceMiB, "pairs": pairs}})
}

// handleForgetShape drops every learned need of a shape (in memory and in
// the database). Audited as forget_need with the shape as its target.
func (s *Server) handleForgetShape(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Shape string `json:"shape"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.Shape == "" {
		proxy.WriteError(w, http.StatusBadRequest, "body must be JSON with a \"shape\" field (from GET /api/needs)", "invalid_request_error", "bad request", nil)
		return
	}
	had, err := s.sched.ForgetShape(r.Context(), req.Shape)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	s.auditRuntime(r, audit.ActionForgetNeed, req.Shape, map[string]any{"forgotten": had, "shape": req.Shape})
	msg := "forgotten for every profile of this shape, on every device count"
	if !had {
		msg = "nothing learned"
	}
	writeJSON(w, http.StatusOK, map[string]any{"msg": msg, "shape": req.Shape, "forgotten": had})
}
