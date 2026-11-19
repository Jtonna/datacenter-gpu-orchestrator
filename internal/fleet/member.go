package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// maxMemMiB bounds a sane memory figure (1 PiB in MiB); anything above is
// treated as unknown.
const maxMemMiB = 1 << 30

var errNoID = errors.New("fleet member has no id")

// memberProblem is one field of a member that was ignored.
type memberProblem struct {
	Field string
	Err   string
}

// UnmarshalJSON decodes a member tolerantly: every field is decoded on its
// own, and a field with the wrong type is ignored (and recorded in
// m.problems) instead of failing the member. Numbers are accepted as int or
// float. Only a missing id makes the member unusable. Unknown fields are
// ignored.
func (m *Member) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = Member{}
	d := &memberDecoder{raw: raw}
	m.ID = d.str("id")
	if m.ID == "" {
		return errNoID
	}
	m.Hostname = d.str("hostname")
	m.Role = d.str("role")
	m.Source = d.str("source")
	m.Online = d.boolean("online")
	if p := d.strPtr("ip"); p != nil {
		m.IP = p
	}
	if f := d.f64("last_seen"); f != nil {
		m.LastSeen = int64(math.Floor(*f))
	}
	if f := d.f64("rpc_port"); f != nil {
		if *f >= 1 && *f <= 65535 && *f == math.Trunc(*f) {
			p := int(*f)
			m.RPCPort = &p
		} else {
			d.bad("rpc_port", fmt.Errorf("%v is not a port", *f))
		}
	}
	m.RAMTotalGB = d.f64("ram_total_gb")
	m.RAMUsedGB = d.f64("ram_used_gb")
	m.VRAMTotal = d.f64("vram_total")
	m.VRAMUsed = d.f64("vram_used")
	if f := d.f64("mem_available_mib"); f != nil {
		if *f < 0 || *f > maxMemMiB {
			d.bad("mem_available_mib", fmt.Errorf("value %v out of range, treated as unknown", *f))
		} else {
			v := int(math.Floor(*f))
			m.MemAvailableMiB = &v
		}
	}
	if f := d.f64("rpc_server_mem_mib"); f != nil {
		if *f < 0 || *f > maxMemMiB {
			d.bad("rpc_server_mem_mib", fmt.Errorf("value %v out of range, treated as unknown", *f))
		} else {
			m.RPCServerMemMiB = f
		}
	}
	m.RPCServerVersion = d.strPtr("rpc_server_version")
	if r, ok := raw["last_recovery_action"]; ok && !isNull(r) {
		var ra map[string]json.RawMessage
		if err := json.Unmarshal(r, &ra); err != nil {
			d.bad("last_recovery_action", err)
		} else {
			rd := &memberDecoder{raw: ra}
			act := &RecoveryAction{Action: rd.str("action"), Outcome: rd.str("outcome"),
				Reason: rd.str("reason"), Nonce: rd.str("nonce")}
			if f := rd.f64("at"); f != nil {
				act.At = int64(math.Floor(*f))
			}
			if f := rd.f64("duration_ms"); f != nil {
				act.DurationMs = int64(math.Floor(*f))
			}
			for _, p := range rd.problems {
				d.bad("last_recovery_action."+p.Field, errors.New(p.Err))
			}
			m.LastRecoveryAction = act
		}
	}
	m.problems = d.problems
	return nil
}

type memberDecoder struct {
	raw      map[string]json.RawMessage
	problems []memberProblem
}

func isNull(r json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(r), []byte("null")) }

func (d *memberDecoder) bad(field string, err error) {
	d.problems = append(d.problems, memberProblem{Field: field, Err: err.Error()})
}

// get returns the raw value, or false when absent or null.
func (d *memberDecoder) get(field string) (json.RawMessage, bool) {
	r, ok := d.raw[field]
	if !ok || isNull(r) {
		return nil, false
	}
	return r, true
}

func (d *memberDecoder) f64(field string) *float64 {
	r, ok := d.get(field)
	if !ok {
		return nil
	}
	var f float64
	if err := json.Unmarshal(r, &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		d.bad(field, fmt.Errorf("want a number, got %s", clip(r)))
		return nil
	}
	return &f
}

func (d *memberDecoder) strPtr(field string) *string {
	r, ok := d.get(field)
	if !ok {
		return nil
	}
	var s string
	if err := json.Unmarshal(r, &s); err != nil {
		d.bad(field, fmt.Errorf("want a string, got %s", clip(r)))
		return nil
	}
	return &s
}

func (d *memberDecoder) str(field string) string {
	if p := d.strPtr(field); p != nil {
		return *p
	}
	return ""
}

func (d *memberDecoder) boolean(field string) bool {
	r, ok := d.get(field)
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(r, &b); err != nil {
		d.bad(field, fmt.Errorf("want a boolean, got %s", clip(r)))
		return false
	}
	return b
}

func clip(r json.RawMessage) string {
	s := string(bytes.TrimSpace(r))
	if len(s) > 40 {
		s = s[:40] + "..."
	}
	return s
}
