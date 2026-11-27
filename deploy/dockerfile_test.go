package deploy

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The base image is a REQUIRED build argument (build-92 B8): no Dockerfile
// names a base image that only one owner's machine has. A build without
// BASE_IMAGE fails in a first small stage with one sentence that says what
// to pass, before the final stage runs anything on the empty placeholder.
// (Docker is not run here; the lead's Linux gate may build for real.)
func TestDockerfilesRequireBaseImage(t *testing.T) {
	for _, name := range []string{"Dockerfile", "rpc-build.Dockerfile", "rpc-build.Dockerfile.stock"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			s := strings.ReplaceAll(string(b), "\r\n", "\n")
			if regexp.MustCompile(`(?m)^ARG BASE_IMAGE=\S`).MatchString(s) {
				t.Errorf("BASE_IMAGE has a default")
			}
			if !regexp.MustCompile(`(?m)^ARG BASE_IMAGE$`).MatchString(s) {
				t.Errorf("no global ARG BASE_IMAGE")
			}
			if strings.Contains(s, "llama-swap-whisper") {
				t.Errorf("names the owner's local image llama-swap-whisper")
			}
			const say = "BASE_IMAGE is required: pass --build-arg BASE_IMAGE=<image>"
			if !strings.Contains(s, say) {
				t.Errorf("no failing sentence %q", say)
			}
			// the final stage: FROM the argument, and its first step needs the check
			final := s[strings.LastIndex(s, "\nFROM "):]
			lines := strings.Split(strings.TrimSpace(final), "\n")
			if lines[0] != "FROM ${BASE_IMAGE:-scratch}" {
				t.Errorf("final stage starts %q", lines[0])
			}
			for _, ln := range lines[1:] {
				ln = strings.TrimSpace(ln)
				if ln == "" || strings.HasPrefix(ln, "#") {
					continue
				}
				if !strings.HasPrefix(ln, "COPY --from=base-image-check ") {
					t.Errorf("the final stage's first step is %q, not the copy from the check stage", ln)
				}
				break
			}
			check := regexp.MustCompile(`(?s)FROM \S+ AS base-image-check\nARG BASE_IMAGE\nRUN if \[ -z "\$BASE_IMAGE" \]; then echo "BASE_IMAGE is required`)
			if !check.MatchString(s) {
				t.Errorf("no base-image-check stage that tests the argument")
			}
		})
	}
}

// The models volume binds a host folder named by POTLUCK_MODELS_DIR, with a
// neutral default; no owner-specific path.
func TestComposeModelsDirIsAVariable(t *testing.T) {
	b, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "device: ${POTLUCK_MODELS_DIR:-/srv/potluck/models}") {
		t.Errorf("the models volume does not bind ${POTLUCK_MODELS_DIR:-/srv/potluck/models}")
	}
	if strings.Contains(s, "llama-swap") {
		t.Errorf("the compose file names llama-swap")
	}
}
