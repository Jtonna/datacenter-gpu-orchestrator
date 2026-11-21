package stats

import (
	"bufio"
	"io"
	"math"
	"strconv"
	"strings"
)

// Sample is one parsed series of a metric family.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Metrics maps a metric name to its samples, in input order. It is never
// modified after Parse returns, so it may be shared between goroutines.
type Metrics map[string][]Sample

// maxLineBytes bounds one exposition line. A longer line fails the scrape.
const maxLineBytes = 1 << 20

// Parse reads the Prometheus text exposition format tolerantly.
//
//   - Blank lines, comments, and HELP/TYPE lines are skipped.
//   - keep (if non-nil) is called with each metric name before its labels are
//     parsed; families it rejects are dropped cheaply (node_exporter on a
//     laptop sends about 2600 lines, of which only a few families are used).
//   - Values are float64 (scientific notation allowed). NaN, +Inf and -Inf
//     mean "unknown": the sample is skipped (not counted as bad).
//   - An optional trailing integer timestamp is ignored.
//   - Decision: a malformed line is skipped and counted in bad; it never fails
//     the whole scrape. Only a read error (or a line over 1 MiB) is returned
//     as an error.
func Parse(r io.Reader, keep func(name string) bool) (m Metrics, bad int, err error) {
	m = Metrics{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		name, s, kept, ok := parseLine(line, keep)
		if !ok {
			bad++
			continue
		}
		if kept {
			m[name] = append(m[name], s)
		}
	}
	return m, bad, sc.Err()
}

func isNameStart(c byte) bool {
	return c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

func skipWS(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

func scanName(s string, i int) int {
	if i >= len(s) || !isNameStart(s[i]) {
		return i
	}
	i++
	for i < len(s) && isNameChar(s[i]) {
		i++
	}
	return i
}

// parseLine parses one sample line. kept is false when the family is
// filtered out or the value is NaN/Inf; ok is false for a malformed line.
func parseLine(line string, keep func(string) bool) (name string, s Sample, kept, ok bool) {
	end := scanName(line, 0)
	if end == 0 {
		return "", s, false, false
	}
	name = line[:end]
	if keep != nil && !keep(name) {
		return name, s, false, true
	}
	i := skipWS(line, end)
	if i < len(line) && line[i] == '{' {
		var labels map[string]string
		labels, i, ok = parseLabels(line, i+1)
		if !ok {
			return name, s, false, false
		}
		s.Labels = labels
	}
	i = skipWS(line, i)
	j := i
	for j < len(line) && line[j] != ' ' && line[j] != '\t' {
		j++
	}
	if j == i {
		return name, s, false, false
	}
	v, err := strconv.ParseFloat(line[i:j], 64)
	if err != nil {
		return name, s, false, false
	}
	rest := strings.TrimSpace(line[j:])
	if rest != "" {
		if _, err := strconv.ParseInt(rest, 10, 64); err != nil {
			return name, s, false, false
		}
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return name, s, false, true
	}
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	s.Value = v
	return name, s, true, true
}

// parseLabels parses `name="value",...}` starting just after '{'. It returns
// the index after the closing brace.
func parseLabels(line string, i int) (map[string]string, int, bool) {
	labels := map[string]string{}
	for {
		i = skipWS(line, i)
		if i < len(line) && line[i] == '}' {
			return labels, i + 1, true
		}
		end := scanName(line, i)
		if end == i {
			return nil, i, false
		}
		key := line[i:end]
		i = skipWS(line, end)
		if i >= len(line) || line[i] != '=' {
			return nil, i, false
		}
		i = skipWS(line, i+1)
		if i >= len(line) || line[i] != '"' {
			return nil, i, false
		}
		i++
		var b strings.Builder
		closed := false
		for i < len(line) {
			c := line[i]
			if c == '\\' && i+1 < len(line) {
				switch line[i+1] {
				case '\\':
					b.WriteByte('\\')
				case '"':
					b.WriteByte('"')
				case 'n':
					b.WriteByte('\n')
				default:
					b.WriteByte('\\')
					b.WriteByte(line[i+1])
				}
				i += 2
				continue
			}
			if c == '"' {
				closed = true
				i++
				break
			}
			b.WriteByte(c)
			i++
		}
		if !closed {
			return nil, i, false
		}
		labels[key] = b.String()
		i = skipWS(line, i)
		if i < len(line) && line[i] == ',' {
			i++
			continue
		}
		if i < len(line) && line[i] == '}' {
			return labels, i + 1, true
		}
		return nil, i, false
	}
}

// Metric families the contract names, per source. Everything else is dropped
// at parse time.
var (
	nodeFamilies = map[string]bool{
		"node_cpu_seconds_total":            true,
		"node_memory_MemTotal_bytes":        true,
		"node_memory_MemAvailable_bytes":    true,
		"node_memory_Cached_bytes":          true,
		"node_load1":                        true,
		"node_load5":                        true,
		"node_load15":                       true,
		"node_network_up":                   true,
		"node_network_receive_bytes_total":  true,
		"node_network_transmit_bytes_total": true,
		"node_network_speed_bytes":          true,
		"node_hwmon_temp_celsius":           true,
		"node_hwmon_sensor_label":           true,
		"node_hwmon_chip_names":             true,
		"node_hwmon_pwm":                    true,
		"node_hwmon_fan_rpm":                true,
		"node_hwmon_freq_freq_mhz":          true,
		"node_hwmon_power_watt":             true,
		"node_drm_card_info":                true,
		"node_drm_gpu_busy_percent":         true,
		"node_drm_memory_vram_size_bytes":   true,
		"node_drm_memory_vram_used_bytes":   true,
		"node_drm_memory_gtt_size_bytes":    true,
		"node_drm_memory_gtt_used_bytes":    true,
	}
	nvidiaFamilies = map[string]bool{
		"nvidia_smi_gpu_info":                         true,
		"nvidia_smi_memory_total_bytes":               true,
		"nvidia_smi_memory_used_bytes":                true,
		"nvidia_smi_memory_reserved_bytes":            true,
		"nvidia_smi_temperature_gpu":                  true,
		"nvidia_smi_clocks_current_graphics_clock_hz": true,
		"nvidia_smi_utilization_gpu_ratio":            true,
		"nvidia_smi_fan_speed_ratio":                  true,
		"nvidia_smi_power_draw_watts":                 true,
	}
)

func keepNode(name string) bool   { return nodeFamilies[name] }
func keepNvidia(name string) bool { return nvidiaFamilies[name] }
