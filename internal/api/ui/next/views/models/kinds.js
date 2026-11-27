// The two kinds of a model's profiles: PRODUCTION and TEST (the owner's
// decision of 25 Nov). The stored stage decides (GET /api/profiles and
// /api/models carry it as stage); a profile that does not say is a test
// when it was generated or is pinned to zones (force_nodes), else
// production: core/ui.js profileStage, the server's config.EffectiveStage.
//
// A model's "profiles" are its production profiles ("2 profiles"); the rest
// is one line of its own ("and 40 test profiles") and is
// never counted with them: the Models rows, the side panel and the delete
// dialog all read it from here.

import { plural, num } from '../../core/format.js';
import { profileStage } from '../../core/ui.js';

// isProduction(profile) -> true for a production profile
export const isProduction = p => !!p && profileStage(p) === 'production';

// splitProfiles(profiles) -> {prod, test}: the two lists, in their order
export function splitProfiles(ps) {
  const prod = [], test = [];
  for (const p of ps || []) (isProduction(p) ? prod : test).push(p);
  return { prod, test };
}

// prodText(n) -> "2 profiles", "1 profile", "No profiles"
export const prodText = n => (n ? plural(n, 'profile') : 'No profiles');
// testText(n) -> "1 test profile", "40 test profiles"
export const testText = n => plural(n, 'test profile');

// A model's need is that of its PRODUCTION profiles (the reviewer, 25 Nov):
// a 2k test configuration must not stand for the model.

// profName(p) -> "qwen3.5-4b at 16k": a profile and its context
export const profName = p => p.id + (p.ctx ? ' at ' + (p.ctx % 1024 === 0 ? p.ctx / 1024 + 'k' : num(p.ctx)) : '');
// needBasis(p) -> "set by hand", "measured" or "estimated": how a profile's
// need is known (needs_written: needs_mib written in the profile, shown
// whatever the plan says, the reviewer's ruling of 26 Nov)
export const needBasis = p => (p.needs_written ? 'set by hand' : p.needs_measured ? 'measured' : 'estimated');

// prodNeeds(m) -> null when no production profile has a need, else
// {lo, hi, basis}: the production profiles with the smallest and largest
// need (the same one when there is one), and "measured", "estimated" or
// "mixed" over the two ends
export function prodNeeds(m) {
  const ps = splitProfiles(m && m.profiles).prod.filter(p => p.needs_mib > 0).sort((a, b) => a.needs_mib - b.needs_mib);
  if (!ps.length) return null;
  const lo = ps[0], hi = ps[ps.length - 1];
  const a = needBasis(lo), b = needBasis(hi);
  return { lo, hi, basis: a === b ? a : 'mixed' };
}

// zonesFor(mib, sums) -> the fewest zones of the empty cluster that hold
// mib (fit.budgets_mib: what one, two, three ... zones give together); 0 when
// the whole cluster is too small; null when the sums are not known
export function zonesFor(mib, sums) {
  if (!Array.isArray(sums) || !sums.length) return null;
  const i = sums.findIndex(s => s >= mib);
  return i < 0 ? 0 : i + 1;
}

// restorePoints(p) -> null for a profile whose model keeps no restore points
// on the hosting machine (a plain family, or no host_memory served), else
// {n, mib, own}: how many it keeps (context checkpoints), their size, and
// whether the profile sets its own number in its extra arguments
// (--ctx-checkpoints; host_memory.checkpoints_from, scheduler hostmem.go).
// The Configurations tables' "Restore points" column and each profile's
// panel read it (the owner's walkthrough, build-92 W1).
export function restorePoints(p) {
  const x = p && p.host_memory;
  if (!x || !x.family || x.family === 'plain' || !(x.checkpoints > 0 || x.checkpoints_from)) return null;
  return { n: x.checkpoints || 0, mib: x.checkpoints_mib, own: /extra arguments/.test(x.checkpoints_from || '') };
}
