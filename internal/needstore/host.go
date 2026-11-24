package needstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// The measured host overhead (phase 6, profile host: node): what a host
// process on a node took there besides its device share, per shape of load,
// per node and per device count (a host holding half the model does other
// work than one holding all of it). One row per (shape, node, devices).
//
// The figure in force follows the keep-the-higher rule (Add): a reading
// replaces it only when it is higher; it comes down only when the llama
// build or the model file changed, or when three consecutive readings are
// all lower (then the highest of the three). The row keeps the last three
// readings with their dates.
//
// Tables and ledgers. needs_host_overhead (ledger needs_host_migrations v1)
// is the release before device counts: one row per (shape, node). It is
// left at version 1 and receives, for one device, the figure in force (a
// write-through), so that release opens the file and reads a figure that
// means what it meant there. The rows by device count live in
// needs_host_by_count, under a ledger of their own
// (needs_host_count_migrations), which that release never looks at. A
// version 2 of needs_host_migrations would not do: that release refuses a
// ledger ahead of the one it knows (migrate), and its whole needs store
// would then not open. Its v1 rows are copied as one-device rows with no
// readings (Pending: their history is read once at start, scheduler
// hostoverhead.go); a reading it writes after a rollback is taken in at the
// next start (importOlder).

// MaxHostOverheadMiB is the largest figure kept (host_overhead_mib's range).
const MaxHostOverheadMiB = 16384

// MaxHostReadings is how many readings a row keeps.
const MaxHostReadings = 3

// HostReading is one reading of the host overhead.
type HostReading struct {
	MiB   int       `json:"mib"`
	At    time.Time `json:"at"`
	Cause string    `json:"cause,omitempty"`
}

// HostOverhead is one stored row. Shape is the profile's shape (ShapeKeyHost
// with host "node"); Node the node id that hosted; Devices the device count
// of the placement. MiB is the figure in force and MeasuredAt the date of
// the reading it came from. LogHostMiB is the log's host buffers at the
// latest reading, shown only. Readings are the last three, newest first; an
// empty list is a row copied from the release before device counts whose
// history is not read yet (Pending).
type HostOverhead struct {
	Shape      string
	Node       string
	Devices    int
	MiB        int
	LogHostMiB int
	MeasuredAt time.Time
	Release    string
	LlamaBuild string
	ModelBytes int64
	Profile    string // the profile that measured the latest reading (for display)
	Cause      string // the cause of the reading in force
	Readings   []HostReading
}

// Pending: a row copied from the release before device counts, whose
// readings are not known yet.
func (h HostOverhead) Pending() bool { return len(h.Readings) == 0 }

// Outcomes of Add.
const (
	HostFirst   = "first"   // no figure before
	HostRaised  = "raised"  // the reading was higher
	HostKept    = "kept"    // lower or equal: the figure in force stays
	HostLowered = "lowered" // three consecutive lower readings: the highest of them
	HostReset   = "reset"   // the llama build or the model file changed
)

// Add applies reading r to h by the keep-the-higher rule; reset says the
// llama build or the model file changed since h's figure. It returns what
// happened to the figure in force.
func (h *HostOverhead) Add(r HostReading, reset bool) string {
	first := h.MiB == 0 && h.MeasuredAt.IsZero() && len(h.Readings) == 0
	h.Readings = append([]HostReading{r}, h.Readings...)
	if len(h.Readings) > MaxHostReadings {
		h.Readings = h.Readings[:MaxHostReadings]
	}
	set := func(x HostReading) { h.MiB, h.MeasuredAt, h.Cause = x.MiB, x.At, x.Cause }
	switch {
	case first:
		set(r)
		return HostFirst
	case reset:
		h.Readings = []HostReading{r}
		set(r)
		return HostReset
	case r.MiB > h.MiB:
		set(r)
		return HostRaised
	}
	if len(h.Readings) < MaxHostReadings {
		return HostKept
	}
	top := h.Readings[0]
	for _, x := range h.Readings {
		if x.MiB >= h.MiB {
			return HostKept
		}
		if x.MiB > top.MiB {
			top = x
		}
	}
	set(top)
	return HostLowered
}

var hostMigrations = []func(*sql.Tx) error{
	execMigration(`
CREATE TABLE needs_host_overhead (
  shape        TEXT NOT NULL,
  node         TEXT NOT NULL,
  mib          INTEGER NOT NULL,
  log_host_mib INTEGER NOT NULL,
  measured_at  INTEGER NOT NULL,
  release      TEXT NOT NULL,
  llama_build  TEXT NOT NULL,
  model_bytes  INTEGER NOT NULL,
  profile      TEXT NOT NULL,
  cause        TEXT NOT NULL,
  PRIMARY KEY (shape, node)
);
`),
}

// hostCountMigrations are needs_host_by_count's; version 1 copies every
// row of needs_host_overhead as the one-device figure, with no readings.
var hostCountMigrations = []func(*sql.Tx) error{
	execMigration(`
CREATE TABLE needs_host_by_count (
  shape        TEXT NOT NULL,
  node         TEXT NOT NULL,
  devices      INTEGER NOT NULL,
  mib          INTEGER NOT NULL,
  log_host_mib INTEGER NOT NULL,
  measured_at  INTEGER NOT NULL,
  release      TEXT NOT NULL,
  llama_build  TEXT NOT NULL,
  model_bytes  INTEGER NOT NULL,
  profile      TEXT NOT NULL,
  cause        TEXT NOT NULL,
  readings     TEXT NOT NULL,
  PRIMARY KEY (shape, node, devices)
);
INSERT INTO needs_host_by_count (shape, node, devices, mib, log_host_mib, measured_at, release, llama_build, model_bytes, profile, cause, readings)
  SELECT shape, node, 1, mib, log_host_mib, measured_at, release, llama_build, model_bytes, profile, cause, '[]' FROM needs_host_overhead;
`),
}

func usableHost(mib int) bool { return mib >= 0 && mib <= MaxHostOverheadMiB }

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

// latest is the date of h's newest reading, else MeasuredAt.
func (h HostOverhead) latest() time.Time {
	if len(h.Readings) > 0 {
		return h.Readings[0].At
	}
	return h.MeasuredAt
}

func putHostRow(tx *sql.Tx, h HostOverhead) error {
	rs := h.Readings
	if rs == nil {
		rs = []HostReading{}
	}
	doc, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO needs_host_by_count (shape, node, devices, mib, log_host_mib, measured_at, release, llama_build, model_bytes, profile, cause, readings)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(shape, node, devices) DO UPDATE SET mib = excluded.mib, log_host_mib = excluded.log_host_mib, measured_at = excluded.measured_at,
		release = excluded.release, llama_build = excluded.llama_build, model_bytes = excluded.model_bytes, profile = excluded.profile,
		cause = excluded.cause, readings = excluded.readings`,
		h.Shape, h.Node, h.Devices, h.MiB, h.LogHostMiB, ms(h.MeasuredAt), h.Release, h.LlamaBuild, h.ModelBytes, h.Profile, h.Cause, string(doc)); err != nil {
		return err
	}
	if h.Devices != 1 {
		return nil
	}
	// The release before device counts reads one row per (shape, node): the
	// one-device figure in force, dated by the newest reading so that its
	// own later readings can be told apart (importOlder).
	_, err = tx.Exec(`INSERT INTO needs_host_overhead (shape, node, mib, log_host_mib, measured_at, release, llama_build, model_bytes, profile, cause)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(shape, node) DO UPDATE SET mib = excluded.mib, log_host_mib = excluded.log_host_mib, measured_at = excluded.measured_at,
		release = excluded.release, llama_build = excluded.llama_build, model_bytes = excluded.model_bytes, profile = excluded.profile, cause = excluded.cause`,
		h.Shape, h.Node, h.MiB, h.LogHostMiB, ms(h.latest()), h.Release, h.LlamaBuild, h.ModelBytes, h.Profile, h.Cause)
	return err
}

// PutHost stores h, replacing the row of its shape, node and device count.
func (s *Store) PutHost(h HostOverhead) error {
	if h.Shape == "" || h.Node == "" || h.Devices < 1 || !usableHost(h.MiB) {
		return fmt.Errorf("host overhead %d MiB for node %q on %d devices: not storable (0 to %d MiB, with a shape, a node and a device count)",
			h.MiB, h.Node, h.Devices, MaxHostOverheadMiB)
	}
	return s.db.Write(func(tx *sql.Tx) error { return putHostRow(tx, h) })
}

func scanHost(rows *sql.Rows) (HostOverhead, error) {
	var h HostOverhead
	var at int64
	var doc string
	err := rows.Scan(&h.Shape, &h.Node, &h.Devices, &h.MiB, &h.LogHostMiB, &at, &h.Release, &h.LlamaBuild, &h.ModelBytes, &h.Profile, &h.Cause, &doc)
	if err != nil {
		return h, err
	}
	h.MeasuredAt = fromMS(at)
	if json.Unmarshal([]byte(doc), &h.Readings) != nil {
		h.Readings = nil
	}
	return h, nil
}

const hostCols = `shape, node, devices, mib, log_host_mib, measured_at, release, llama_build, model_bytes, profile, cause, readings`

// AllHost returns every usable row, ordered by shape, node and device count.
// A row whose figure is out of range is skipped (and left in place).
func (s *Store) AllHost() ([]HostOverhead, error) {
	rows, err := s.db.DB().Query(`SELECT ` + hostCols + ` FROM needs_host_by_count ORDER BY shape, node, devices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostOverhead
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		if usableHost(h.MiB) && h.Devices >= 1 {
			out = append(out, h)
		}
	}
	return out, rows.Err()
}

// importOlder takes in, as a reading of the one-device row, every row the
// release before device counts wrote after this build's last write (a
// rollback that measured): newer than the one-device row's newest reading.
func importOlder(tx *sql.Tx) error {
	type old struct {
		shape, node, release, build, profile, cause string
		mib, logHost                                int
		at, bytes                                   int64
	}
	rows, err := tx.Query(`SELECT shape, node, mib, log_host_mib, measured_at, release, llama_build, model_bytes, profile, cause FROM needs_host_overhead`)
	if err != nil {
		return err
	}
	var olds []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.shape, &o.node, &o.mib, &o.logHost, &o.at, &o.release, &o.build, &o.bytes, &o.profile, &o.cause); err != nil {
			rows.Close()
			return err
		}
		olds = append(olds, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range olds {
		if !usableHost(o.mib) {
			continue
		}
		r, err := tx.Query(`SELECT `+hostCols+` FROM needs_host_by_count WHERE shape = ? AND node = ? AND devices = 1`, o.shape, o.node)
		if err != nil {
			return err
		}
		var h HostOverhead
		found := false
		if r.Next() {
			h, err = scanHost(r)
			found = true
		}
		r.Close()
		if err != nil {
			return err
		}
		if found && !fromMS(o.at).After(h.latest()) {
			continue // this build's own write-through, or older
		}
		if !found {
			h = HostOverhead{Shape: o.shape, Node: o.node, Devices: 1}
		}
		reset := found && (h.LlamaBuild != o.build || (h.ModelBytes > 0 && o.bytes > 0 && h.ModelBytes != o.bytes))
		h.Add(HostReading{MiB: o.mib, At: fromMS(o.at), Cause: o.cause}, reset)
		h.LogHostMiB, h.Release, h.LlamaBuild, h.ModelBytes, h.Profile = o.logHost, o.release, o.build, o.bytes, o.profile
		if err := putHostRow(tx, h); err != nil {
			return err
		}
	}
	return nil
}
