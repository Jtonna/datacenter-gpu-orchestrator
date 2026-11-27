package config

import "testing"

// build-92 B6: the built-in node source is agents (potluck-agent
// registrations). fleet stays a stored, documented legacy value: a stored
// fleet still reads fleet, a stored agents is unchanged.
func TestMembershipDefaultIsAgents(t *testing.T) {
	opts := Options{FileExists: func(string) bool { return true }}
	yaml := func(t *testing.T, y string) string {
		t.Helper()
		c, err := ParseWithOptions([]byte(y), opts)
		if err != nil {
			t.Fatal(err)
		}
		return c.Defaults.Membership
	}
	doc := func(t *testing.T, j string) string {
		t.Helper()
		d, err := ParseDocument([]byte(j))
		if err != nil {
			t.Fatal(err)
		}
		c, err := Build(d, Bootstrap{}, opts)
		if err != nil {
			t.Fatal(err)
		}
		return c.Defaults.Membership
	}
	if got := DefaultDefaults().Membership; got != MembershipAgents {
		t.Errorf("built-in default %q, want agents", got)
	}
	for _, c := range []struct{ name, got, want string }{
		{"yaml without the key", yaml(t, "profiles: {}\n"), MembershipAgents},
		{"yaml without the key, with a fleet url", yaml(t, "fleet: {url: \"http://192.0.2.1:8090\"}\nprofiles: {}\n"), MembershipAgents},
		{"yaml stored fleet", yaml(t, "defaults: {membership: fleet}\nprofiles: {}\n"), MembershipFleet},
		{"yaml stored agents", yaml(t, "defaults: {membership: agents}\nprofiles: {}\n"), MembershipAgents},
		{"document without the key", doc(t, `{"settings": {"max_pool": 4}}`), MembershipAgents},
		{"document stored fleet", doc(t, `{"settings": {"membership": "fleet"}}`), MembershipFleet},
		{"document stored agents", doc(t, `{"settings": {"membership": "agents"}}`), MembershipAgents},
	} {
		if c.got != c.want {
			t.Errorf("%s: membership %q, want %q", c.name, c.got, c.want)
		}
	}
}
