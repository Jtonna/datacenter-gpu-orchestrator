// Package deploy holds the deployment files; this test checks the compose
// file without Docker.
package deploy

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type composeFile struct {
	Services map[string]struct {
		Image         string            `yaml:"image"`
		ContainerName string            `yaml:"container_name"`
		Command       []string          `yaml:"command"`
		Environment   map[string]string `yaml:"environment"`
		Ports         []string          `yaml:"ports"`
		Expose        []string          `yaml:"expose"`
		MemLimit      string            `yaml:"mem_limit"`
		Volumes       []string          `yaml:"volumes"`
		Networks      []string          `yaml:"networks"`
	} `yaml:"services"`
}

// The log store of central logging (CR-10): the pinned image, no published
// port, on the scheduler's network, with a memory limit, its data in a
// folder and the flags Potluck's settings describe; the scheduler has its
// address.
func TestComposeLogStore(t *testing.T) {
	b, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var c composeFile
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatalf("the compose file does not parse: %v", err)
	}
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
	if strings.Join(st.Networks, ",") != "sched" {
		t.Errorf("networks %v, want the scheduler's (sched)", st.Networks)
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
	sched := c.Services["llm-scheduler"]
	if got := sched.Environment["LLM_SCHEDULER_LOGS_STORE_URL"]; got != "http://potluck-logs:9428/logs" {
		t.Errorf("LLM_SCHEDULER_LOGS_STORE_URL = %q", got)
	}
}
