// Central logging (CR-10): the "Logs" entry and the links that open the log
// store's own search page with a query filled in. Potluck serves that page
// read only under /logs/ (the store's search page is /logs/select/vmui/).
// It opens in a new tab: it is a whole application of its own, with its own
// address bar state, and in a new tab every part of it keeps working.
//
// Nothing here is shown while logging is off: the status has a top-level
// `logs` object with `enabled` and `store`, and the page is there only when
// both are true.
//
// The link format is the search page's own (VictoriaLogs v1.54.0, vmui
// source app/vmui/packages/vmui/src): a hash route "#/?..." with
//   query               the LogsQL query (pages/QueryPage/hooks/useQueryController.ts, constants/logs.ts)
//   g0.end_input        the end of the time range, an ISO time (pages/QueryPage/hooks/useTimePeriod.ts)
//   g0.range_input      its length, for example 3600s (utils/time/duration.ts)
//   g0.relative_time    "none" for a fixed range, or a preset id such as last_1_hour (utils/time/relative.ts)

import { h } from './dom.js';

export const LOGS_PAGE = '/logs/select/vmui/';

// logsOn(status) -> whether the store's search page is there.
export const logsOn = st => !!(st && st.logs && st.logs.enabled && st.logs.store);

// quote(v) -> v as a LogsQL quoted string.
export const quote = v => '"' + String(v).replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"';

// The queries. ":=" is an exact match of the whole field value.
export const nodeQuery = id => 'node_id:=' + quote(id);
export const profileQuery = name => 'profile:=' + quote(name);

// logsURL(query, from, to) -> the search page with the query and a time range
// filled in. from and to are unix ms; without from the range is the last
// hour; without to it ends now.
export function logsURL(query, from, to) {
  const p = new URLSearchParams();
  p.set('query', query || '*');
  if (from != null) {
    const end = to != null ? to : Date.now();
    p.set('g0.end_input', new Date(end).toISOString());
    p.set('g0.range_input', Math.max(1, Math.ceil((end - from) / 1000)) + 's');
    p.set('g0.relative_time', 'none');
  } else {
    p.set('g0.relative_time', 'last_1_hour');
  }
  return LOGS_PAGE + '#/?' + p.toString();
}

// A minute either side of a window, so the lines just before and after it
// are there too.
const PAD_MS = 60 * 1000;

// window(from, to) -> [from, to] widened by PAD_MS; to null stays "now".
export const padded = (from, to) => [from - PAD_MS, to != null ? to + PAD_MS : null];

// logsLink(st, label, query, from, to, title) -> a link that opens the search
// page in a new tab, or null while logging is off.
export function logsLink(st, label, query, from, to, title) {
  if (!logsOn(st)) return null;
  return h('a.logs-link', { href: logsURL(query, from, to), target: '_blank', rel: 'noopener', title: title || 'Open the log search in a new tab' }, label);
}
