package api

import (
	"context"
	"net/http"
	"sort"
	"strconv"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/availability"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/groups"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
)

// GET /api/nodes/uptime?days=7: uptime over the last days (1 to 90,
// default 7) per zone, per group, for the zones in no group and for the
// fleet, from the availability hours (availability.Uptimes has the
// formula: up = free + in use + loading over up + lost + set aside;
// switched-off time is left out and given as off_s).
//
// It is not the uptime of /api/nodes/availability, which is reachable
// over known time (a paused zone that answers counts up there, and its
// switched-off hours count in the known time); that field keeps its
// meaning.

const (
	uptimeDefaultDays = 7
	uptimeMaxDays     = 90
)

// ZoneUptime is one zone's totals with its group.
type ZoneUptime struct {
	availability.MachineTotals
	Group string `json:"group,omitempty"`
}

// GroupUptime is one group's totals (its members' seconds summed).
type GroupUptime struct {
	Name string `json:"name"`
	availability.Totals
}

// UptimeDoc is the answer of GET /api/nodes/uptime.
type UptimeDoc struct {
	Window    availability.Window `json:"window"`
	Zones     []ZoneUptime        `json:"zones"`
	Groups    []GroupUptime       `json:"groups"`
	Ungrouped availability.Totals `json:"ungrouped"`
	Fleet     availability.Totals `json:"fleet"`
}

// uptimeDoc reads the totals and sums them per group. A store that cannot
// be read answers the window with no figures (logged), never an error.
func (s *Server) uptimeDoc(ctx context.Context, days int) UptimeDoc {
	w, m, err := availability.Uptimes(ctx, s.opts.Usage, s.avail, days, s.opts.Now())
	if err != nil {
		s.log.Warn("uptime: the availability hours could not be read", "err", err)
	}
	return sumUptime(w, m, s.groups.Memberships(), s.groups.List(), s.removedZones())
}

// removedZones is the ids of the removed machines (pool facts).
func (s *Server) removedZones() map[string]bool {
	out := map[string]bool{}
	if s.opts.Pool == nil {
		return out
	}
	for _, f := range s.opts.Pool.Facts() {
		if f.State == pool.StateRemoved {
			out[f.ID] = true
		}
	}
	return out
}

// sumUptime sums the machines' totals per group, ungrouped and for the
// fleet. A removed machine is left out everywhere (it has no uptime), as
// the counts leave it out.
func sumUptime(w availability.Window, m map[string]*availability.MachineTotals, of map[string]string, list []groups.Group, removed map[string]bool) UptimeDoc {
	doc := UptimeDoc{Window: w, Zones: []ZoneUptime{}, Groups: []GroupUptime{}}
	byGroup := map[string]*availability.Totals{}
	for _, g := range list {
		byGroup[g.Name] = &availability.Totals{}
	}
	for _, mt := range m {
		if removed[mt.ID] {
			continue
		}
		z := ZoneUptime{MachineTotals: *mt, Group: of[mt.ID]}
		if t := byGroup[z.Group]; t != nil {
			t.Add(mt.Totals)
		} else {
			doc.Ungrouped.Add(mt.Totals)
		}
		doc.Fleet.Add(mt.Totals)
		z.Finish()
		doc.Zones = append(doc.Zones, z)
	}
	sort.Slice(doc.Zones, func(i, j int) bool {
		if doc.Zones[i].Name != doc.Zones[j].Name {
			return doc.Zones[i].Name < doc.Zones[j].Name
		}
		return doc.Zones[i].ID < doc.Zones[j].ID
	})
	for _, g := range list {
		t := byGroup[g.Name]
		t.Finish()
		doc.Groups = append(doc.Groups, GroupUptime{Name: g.Name, Totals: *t})
	}
	doc.Ungrouped.Finish()
	doc.Fleet.Finish()
	return doc
}

func (s *Server) handleUptime(w http.ResponseWriter, r *http.Request) {
	days := uptimeDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > uptimeMaxDays {
			apiErr(w, http.StatusBadRequest, "days is a whole number from 1 to %d", uptimeMaxDays)
			return
		}
		days = n
	}
	writeJSON(w, http.StatusOK, s.uptimeDoc(r.Context(), days))
}
