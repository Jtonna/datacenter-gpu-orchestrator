// Zone groups (v43, internal/api/groups.go): the writes the Zones pages
// make. Every write carries the comment in its body and in X-Comment (the
// audit entry keeps it) and is signed by the default user (no X-Actor).
//
//   POST   /api/groups                {name, members, comment}  create (members move into it)
//   PATCH  /api/groups/{name}         {name, comment}           rename
//   DELETE /api/groups/{name}         {comment}                 delete (its zones then have no group)
//   POST   /api/groups/{name}/members {nodes, comment}          add or move zones into it
//   DELETE /api/groups/{name}/members {nodes, comment}          take zones out of it
//   POST   /api/groups/{name}/pause   {comment}                 every member stops accepting models
//   POST   /api/groups/{name}/unpause {comment}                 every member accepts models again

// A group name: 1 to 64 letters, digits, '.', '_' or '-', starting with a
// letter or digit (internal/groups nameRE); unique without regard to case.
export const GROUP_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
export const GROUP_NAME_RULE = "A group name is 1 to 64 letters, digits, '.', '_' or '-', and starts with a letter or digit.";

export const groupPath = (name, rest = '') => '/api/groups/' + encodeURIComponent(name) + rest;

function need(comment) {
  const c = String(comment || '').trim();
  if (!c) throw new Error('A comment is required.');
  return c;
}

export async function createGroup(api, name, members, comment) {
  const c = need(comment);
  if (!GROUP_NAME.test(name)) throw new Error(GROUP_NAME_RULE);
  return api.post('/api/groups', { name, members, comment: c }, { comment: c });
}
export async function addMembers(api, name, nodes, comment) {
  const c = need(comment);
  return api.post(groupPath(name, '/members'), { nodes, comment: c }, { comment: c });
}
export async function removeMembers(api, name, nodes, comment) {
  const c = need(comment);
  return api.send('DELETE', groupPath(name, '/members'), { nodes, comment: c }, { comment: c });
}
export async function renameGroup(api, name, to, comment) {
  const c = need(comment);
  if (!GROUP_NAME.test(to)) throw new Error(GROUP_NAME_RULE);
  return api.patch(groupPath(name), { name: to, comment: c }, { comment: c });
}
export async function deleteGroup(api, name, comment) {
  const c = need(comment);
  return api.send('DELETE', groupPath(name), { comment: c }, { comment: c });
}
export async function pauseGroup(api, name, pause, comment) {
  const c = need(comment);
  return api.post(groupPath(name, pause ? '/pause' : '/unpause'), { comment: c }, { comment: c });
}
