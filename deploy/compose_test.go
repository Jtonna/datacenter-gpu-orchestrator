// Package deploy holds the deployment files; this test checks the compose
// file without Docker.
package deploy

import (
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

type composeService struct {
	Image         string            `yaml:"image"`
	ContainerName string            `yaml:"container_name"`
	Restart       string            `yaml:"restart"`
	Command       []string          `yaml:"command"`
	Environment   map[string]string `yaml:"environment"`
	Ports         []string          `yaml:"ports"`
	Expose        []string          `yaml:"expose"`
	ExtraHosts    []string          `yaml:"extra_hosts"`
	MemLimit      string            `yaml:"mem_limit"`
	MacAddress    string            `yaml:"mac_address"`
	GPUs          string            `yaml:"gpus"`
	Profiles      []string          `yaml:"profiles"`
	NetworkMode   string            `yaml:"network_mode"`
	Volumes       []string          `yaml:"volumes"`
	// Networks is a list of names or a map of name to {ipv4_address}.
	Networks yaml.Node `yaml:"networks"`
}

type composeFile struct {
	Name     string                    `yaml:"name"`
	Services map[string]composeService `yaml:"services"`
	Volumes  map[string]any            `yaml:"volumes"`
	Networks map[string]struct {
		IPAM struct {
			Config []struct {
				Subnet  string `yaml:"subnet"`
				IPRange string `yaml:"ip_range"`
			} `yaml:"config"`
		} `yaml:"ipam"`
	} `yaml:"networks"`
}

func loadCompose(t *testing.T) composeFile {
	t.Helper()
	b, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var c composeFile
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatalf("the compose file does not parse: %v", err)
	}
	return c
}

// networks gives a service's networks and their fixed addresses ("" when none).
func (s composeService) networks(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	switch s.Networks.Kind {
	case 0:
	case yaml.SequenceNode:
		var l []string
		if err := s.Networks.Decode(&l); err != nil {
			t.Fatal(err)
		}
		for _, n := range l {
			out[n] = ""
		}
	case yaml.MappingNode:
		var m map[string]struct {
			IPv4 string `yaml:"ipv4_address"`
		}
		if err := s.Networks.Decode(&m); err != nil {
			t.Fatal(err)
		}
		for n, v := range m {
			out[n] = v.IPv4
		}
	default:
		t.Fatalf("networks: unexpected YAML kind %v", s.Networks.Kind)
	}
	return out
}

// hostPath is the host side of a bind volume, or "" for a named volume.
func hostPath(v string) string {
	src := v
	if strings.HasPrefix(v, "${") { // ${VAR:-default}/x:/target: the default may hold no colon of its own
		end := strings.Index(v, "}") + 1
		src = v[:end] + strings.SplitN(v[end:], ":", 2)[0]
	} else {
		src = strings.SplitN(v, ":", 2)[0]
	}
	if strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") || strings.HasPrefix(src, "$") {
		return src
	}
	return ""
}

// The log store of central logging (CR-10): the pinned image, no published
// port, on the scheduler's network, with a memory limit, its data in a
// folder and the flags Potluck's settings describe; the scheduler has its
// address.
func TestComposeLogStore(t *testing.T) {
	c := loadCompose(t)
	st, ok := c.Services["potluck-logs"]
	if !ok {
		t.Fatal("no potluck-logs service")
	}
	const image = "victoriametrics/victoria-logs:v1.54.0@sha256:ee4e820cac3721f5f5d91641b950c22439593adc4224f9655e5333d5a84c1dfe"
	if st.Image != image {
		t.Errorf("image %q, want %q", st.Image, image)
	}
	if len(st.Ports) != 0 || len(st.Expose) != 0 {
		t.Errorf("the store's port must not be published: ports %v expose %v", st.Ports, st.Expose)
	}
	if nets := st.networks(t); len(nets) != 1 || nets["sched"] != "" {
		t.Errorf("networks %v, want the scheduler's (sched)", nets)
	}
	if st.MemLimit != "512m" {
		t.Errorf("mem_limit %q", st.MemLimit)
	}
	want := map[string]bool{"-http.pathPrefix=/logs": true, "-retentionPeriod=14d": true, "-retention.maxDiskSpaceUsageBytes=20GiB": true, "-storageDataPath=/storage": true}
	for _, f := range st.Command {
		delete(want, f)
	}
	if len(want) != 0 {
		t.Errorf("flags missing: %v (have %v)", want, st.Command)
	}
	data := false
	for _, v := range st.Volumes {
		data = data || strings.HasSuffix(v, ":/storage")
	}
	if !data {
		t.Errorf("no volume for /storage: %v", st.Volumes)
	}
	sched := c.Services["potluck"]
	if got := sched.Environment["LLM_SCHEDULER_LOGS_STORE_URL"]; got != "http://potluck-logs:9428/logs" {
		t.Errorf("LLM_SCHEDULER_LOGS_STORE_URL = %q", got)
	}
}

const rootVar = "${POTLUCK_ROOT:-/srv/potluck}/"

// The standing layout: project potluck; services and containers potluck,
// potluck-share and potluck-logs, each restarted unless stopped; nothing the
// fleet console needed (no extra_hosts); the network keeps its subnet and
// the scheduler a fixed address; every host folder of the deployment is under
// POTLUCK_ROOT.
func TestComposeStandingLayout(t *testing.T) {
	c := loadCompose(t)
	if c.Name != "potluck" {
		t.Errorf("project name %q, want potluck", c.Name)
	}
	for _, name := range []string{"potluck", "potluck-share", "potluck-logs"} {
		s, ok := c.Services[name]
		if !ok {
			t.Errorf("no service %s", name)
			continue
		}
		if s.ContainerName != name {
			t.Errorf("%s: container_name %q", name, s.ContainerName)
		}
		if s.Restart != "unless-stopped" {
			t.Errorf("%s: restart %q", name, s.Restart)
		}
		if len(s.Profiles) != 0 {
			t.Errorf("%s: in profiles %v, so it would not start by default", name, s.Profiles)
		}
	}
	if len(c.Services) != 4 {
		t.Errorf("%d services, want the three and the optional agent", len(c.Services))
	}
	for name, s := range c.Services {
		if len(s.ExtraHosts) != 0 {
			t.Errorf("%s: extra_hosts %v (only the fleet console needed host.docker.internal)", name, s.ExtraHosts)
		}
		if s.NetworkMode != "" {
			t.Errorf("%s: network_mode %q, want the compose network", name, s.NetworkMode)
		}
		if nets := s.networks(t); len(nets) != 1 {
			t.Errorf("%s: networks %v, want sched only", name, nets)
		} else if _, ok := nets["sched"]; !ok {
			t.Errorf("%s: networks %v, want sched only", name, nets)
		}
		for _, v := range s.Volumes {
			switch p := hostPath(v); {
			case p == "":
			case p == "./seed-config.yaml" || p == "./share-nfs.conf" || p == "/lib/modules": // ship with this file; the host's kernel modules
			case !strings.HasPrefix(p, rootVar):
				t.Errorf("%s: host path %q is not under %s", name, p, rootVar)
			}
		}
	}
	if got := c.Services["potluck"].networks(t)["sched"]; got != "172.30.0.2" {
		t.Errorf("the scheduler's address %q, want 172.30.0.2", got)
	}
	sub := c.Networks["sched"].IPAM.Config
	if len(sub) != 1 || sub[0].Subnet != "172.30.0.0/24" {
		t.Errorf("sched subnet %+v, want 172.30.0.0/24 (a host firewall rule names it)", sub)
	}
	// Every folder of the layout is mounted from the root.
	for dir, svc := range map[string]string{"programs": "potluck", "data": "potluck", "history": "potluck", "child-logs": "potluck", "logs-store": "potluck-logs"} {
		found := false
		for _, v := range c.Services[svc].Volumes {
			found = found || strings.HasPrefix(v, rootVar+dir+":")
		}
		if !found {
			t.Errorf("%s: no volume from %s%s", svc, rootVar, dir)
		}
	}
	sched := c.Services["potluck"]
	if !strings.Contains(strings.Join(sched.Volumes, " "), rootVar+"programs:/var/lib/llm-scheduler/programs:ro") ||
		sched.Environment["LLM_SCHEDULER_PROGRAMS_DIR"] != "/var/lib/llm-scheduler/programs" {
		t.Error("the programs folder is not served read only to the scheduler")
	}
	if p := sched.Ports; len(p) != 1 || !strings.HasSuffix(p[0], ":8080") {
		t.Errorf("scheduler ports %v", p)
	}
}

// The optional agent on the scheduler's host: off by default (a profile), on
// the compose network with a fixed address and MAC (its node id comes from
// the MAC), the GPUs, the models read only, its own data volume, and
// Potluck's address and in-container port on that network.
func TestComposeLocalAgent(t *testing.T) {
	c := loadCompose(t)
	a, ok := c.Services["potluck-agent"]
	if !ok {
		t.Fatal("no potluck-agent service")
	}
	if strings.Join(a.Profiles, ",") != "local-agent" {
		t.Errorf("profiles %v, want [local-agent]", a.Profiles)
	}
	if a.Image != "potluck-agent:"+agentproto.Release {
		t.Errorf("image %q, want potluck-agent:%s", a.Image, agentproto.Release)
	}
	if got := a.networks(t)["sched"]; got != "172.30.0.10" {
		t.Errorf("address %q, want 172.30.0.10", got)
	}
	mac, err := net.ParseMAC(a.MacAddress)
	if err != nil || len(mac) != 6 || mac[0]&0x02 == 0 || mac[0]&0x01 != 0 {
		t.Errorf("mac_address %q: want a fixed, locally administered unicast MAC", a.MacAddress)
	}
	if a.GPUs != "all" {
		t.Errorf("gpus %q", a.GPUs)
	}
	if got := strings.Join(a.Command, " "); got != "--potluck 172.30.0.2:8080 --model-source dir:/models --install docker" {
		t.Errorf("command %q", got)
	}
	vols := strings.Join(a.Volumes, " ")
	if !strings.Contains(vols, "models:/models:ro") || !strings.Contains(vols, "potluck-agent-data:/var/lib/potluck-agent") {
		t.Errorf("volumes %v", a.Volumes)
	}
	if _, ok := c.Volumes["potluck-agent-data"]; !ok {
		t.Error("no named volume potluck-agent-data")
	}
	if a.Restart != "unless-stopped" {
		t.Errorf("restart %q", a.Restart)
	}
}

// Containers without a fixed address take theirs from a dynamic range that
// holds no fixed address, inside the subnet: a container that starts first
// cannot take the scheduler's or the agent's address.
func TestComposeFixedAddressesOutsideDynamicRange(t *testing.T) {
	c := loadCompose(t)
	cfg := c.Networks["sched"].IPAM.Config
	if len(cfg) != 1 {
		t.Fatalf("sched ipam config %+v", cfg)
	}
	subnet, err := netip.ParsePrefix(cfg[0].Subnet)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := netip.ParsePrefix(cfg[0].IPRange)
	if err != nil {
		t.Fatalf("sched has no ip_range for dynamic addresses (%q): %v", cfg[0].IPRange, err)
	}
	if !subnet.Contains(dyn.Addr()) || dyn.Bits() < subnet.Bits() {
		t.Errorf("ip_range %s is not inside the subnet %s", dyn, subnet)
	}
	fixed := 0
	for name, s := range c.Services {
		a := s.networks(t)["sched"]
		if a == "" {
			continue
		}
		fixed++
		ip, err := netip.ParseAddr(a)
		if err != nil {
			t.Errorf("%s: address %q: %v", name, a, err)
			continue
		}
		if !subnet.Contains(ip) {
			t.Errorf("%s: address %s is outside the subnet %s", name, ip, subnet)
		}
		if dyn.Contains(ip) {
			t.Errorf("%s: fixed address %s is inside the dynamic range %s", name, ip, dyn)
		}
	}
	if fixed < 2 {
		t.Errorf("%d fixed addresses, want the scheduler's and the agent's", fixed)
	}
}

// The share's NFSv4 grace and lease times: a file next to the compose file,
// mounted read only at /etc/nfs.conf, with both at 10 s (the kernel minimum),
// so a client's read stands still about 16 s after the share restarts instead
// of about 93 s with the kernel defaults (90 s each).
func TestComposeShareGraceTime(t *testing.T) {
	c := loadCompose(t)
	s, ok := c.Services["potluck-share"]
	if !ok {
		t.Fatal("no potluck-share service")
	}
	mounted := false
	for _, v := range s.Volumes {
		mounted = mounted || v == "./share-nfs.conf:/etc/nfs.conf:ro"
	}
	if !mounted {
		t.Errorf("share-nfs.conf is not mounted read only at /etc/nfs.conf: %v", s.Volumes)
	}
	b, err := os.ReadFile("share-nfs.conf")
	if err != nil {
		t.Fatal(err)
	}
	section := ""
	got := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.TrimSpace(line[1 : len(line)-1])
		case section == "nfsd":
			if k, v, ok := strings.Cut(line, "="); ok {
				got[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	for _, k := range []string{"grace-time", "lease-time"} {
		if got[k] != "10" {
			t.Errorf("[nfsd] %s = %q, want 10", k, got[k])
		}
	}
}
