package bench

import "strings"

// ActiveRunsNaming lists the ids of the runs that are queued, running or
// paused and touch the model file: a step on the file, a step on one of
// profiles, or (a run whose steps are not written yet) a suite snapshot that
// names the file or one of profiles. A model file is not deleted while one
// does (models slice 3, Q19). It errs on the side of naming a run.
func (s *Store) ActiveRunsNaming(file string, profiles []string) ([]string, error) {
	q := `SELECT id FROM runs WHERE state IN (?,?,?) AND (
	  id IN (SELECT run_id FROM run_steps WHERE model_file = ?` + orProfileIn(len(profiles)) + `)
	  OR instr(COALESCE(suite,''), ?) > 0`
	args := []any{RunQueued, RunRunning, RunPaused, file}
	for _, p := range profiles {
		args = append(args, p)
	}
	args = append(args, `"`+file+`"`)
	for _, p := range profiles {
		q += ` OR instr(COALESCE(suite,''), ?) > 0`
		args = append(args, `"`+p+`"`)
	}
	q += `) ORDER BY queued_at, id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func orProfileIn(n int) string {
	if n == 0 {
		return ""
	}
	return ` OR profile IN (?` + strings.Repeat(`,?`, n-1) + `)`
}
