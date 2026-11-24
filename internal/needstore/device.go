package needstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// The measured per-device overhead: what one device of a placement held
// beyond the estimator's share of the weights and KV cache and the compute
// buffer that grows with the context (what defaults.device_overhead_mib
// stands for), per shape of load, per node and per device count. One row
// per (shape, node, devices). It is read on worker nodes only (scheduler
// deviceoverhead.go).
//
// The row is the host overhead's (HostOverhead, with the same Add, the
// keep-the-higher rule): LogHostMiB there holds the log's exact part of the
// reading (the floor no memory noise can push the reading below).
//
// Table needs_device_overhead lives under a ledger of its own,
// needs_device_migrations; releases before it never look at it and open
// the file as before. Nothing existing changes.

// MaxDeviceOverheadMiB is the largest reading kept.
const MaxDeviceOverheadMiB = 4096

// DeviceOverhead is one stored row (see above).
type DeviceOverhead = HostOverhead

var deviceMigrations = []func(*sql.Tx) error{
	execMigration(`
CREATE TABLE needs_device_overhead (
  shape        TEXT NOT NULL,
  node         TEXT NOT NULL,
  devices      INTEGER NOT NULL,
  mib          INTEGER NOT NULL,
  log_part_mib INTEGER NOT NULL,
  measured_at  INTEGER NOT NULL,
  release      TEXT NOT NULL,
  llama_build  TEXT NOT NULL,
  model_bytes  INTEGER NOT NULL,
  profile      TEXT NOT NULL,
  cause        TEXT NOT NULL,
  readings     TEXT NOT NULL,
  PRIMARY KEY (shape, node, devices)
);
`),
}

func usableDevice(mib int) bool { return mib >= 0 && mib <= MaxDeviceOverheadMiB }

// PutDevice stores d, replacing the row of its shape, node and device count.
func (s *Store) PutDevice(d DeviceOverhead) error {
	if d.Shape == "" || d.Node == "" || d.Devices < 1 || !usableDevice(d.MiB) {
		return fmt.Errorf("device overhead %d MiB for node %q on %d devices: not storable (0 to %d MiB, with a shape, a node and a device count)",
			d.MiB, d.Node, d.Devices, MaxDeviceOverheadMiB)
	}
	rs := d.Readings
	if rs == nil {
		rs = []HostReading{}
	}
	doc, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO needs_device_overhead (shape, node, devices, mib, log_part_mib, measured_at, release, llama_build, model_bytes, profile, cause, readings)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(shape, node, devices) DO UPDATE SET mib = excluded.mib, log_part_mib = excluded.log_part_mib, measured_at = excluded.measured_at,
		release = excluded.release, llama_build = excluded.llama_build, model_bytes = excluded.model_bytes, profile = excluded.profile,
		cause = excluded.cause, readings = excluded.readings`,
			d.Shape, d.Node, d.Devices, d.MiB, d.LogHostMiB, ms(d.MeasuredAt), d.Release, d.LlamaBuild, d.ModelBytes, d.Profile, d.Cause, string(doc))
		return err
	})
}

// AllDevice returns every usable row, ordered by shape, node and device
// count. A row whose figure is out of range is skipped (and left in place).
func (s *Store) AllDevice() ([]DeviceOverhead, error) {
	rows, err := s.db.DB().Query(`SELECT shape, node, devices, mib, log_part_mib, measured_at, release, llama_build, model_bytes, profile, cause, readings
		FROM needs_device_overhead ORDER BY shape, node, devices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceOverhead
	for rows.Next() {
		d, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		if usableDevice(d.MiB) && d.Devices >= 1 {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

// DeleteDeviceRow drops the row of one shape, node and device count; no row
// is not an error.
func (s *Store) DeleteDeviceRow(shape, node string, devices int) error {
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM needs_device_overhead WHERE shape = ? AND node = ? AND devices = ?`, shape, node, devices)
		return err
	})
}

// DeleteDevice drops node's rows (every shape and device count); it
// reports how many there were.
func (s *Store) DeleteDevice(node string) (int, error) {
	n := 0
	err := s.db.Write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM needs_device_overhead WHERE node = ?`, node)
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		n = int(c)
		return nil
	})
	return n, err
}
