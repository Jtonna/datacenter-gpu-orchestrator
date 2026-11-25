package modelfiles

import (
	"strings"
	"testing"
)

// Sample /proc/self/mountinfo lines (proc(5)): an overlay root, a bind mount
// read-only, a bind mount read-write, nested mounts, a mount point with a
// space (octal escape), and a file system mounted read-only in its super
// options while the mount entry says rw.
const sampleMountinfo = `1500 1400 0:52 / / rw,relatime master:1 - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w
1501 1500 0:55 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
1510 1500 8:1 /srv/models /models ro,relatime - ext4 /dev/sda1 rw
1511 1500 8:1 /srv/data /var/lib/llm-scheduler/data rw,relatime - ext4 /dev/sda1 rw
1520 1500 8:2 / /mnt/share rw,relatime - ext4 /dev/sdb1 rw
1521 1520 8:3 / /mnt/share/inner ro,relatime - ext4 /dev/sdc1 rw
1522 1521 8:4 / /mnt/share/inner/deeper rw,relatime - xfs /dev/sdd1 rw
1530 1500 8:5 / /mnt/with\040space rw,relatime - ext4 /dev/sde1 rw
1540 1500 8:6 / /mnt/superro rw,relatime - ext4 /dev/sdf1 ro
1550 1500 8:7 / /stacked rw,relatime - ext4 /dev/sdg1 rw
1551 1500 8:8 / /stacked ro,relatime - ext4 /dev/sdh1 rw
`

func TestMountReadOnly(t *testing.T) {
	for _, c := range []struct {
		dir, mount string
		ro         bool
	}{
		{"/models", "/models", true},                                          // the bind mount :ro
		{"/models/sub", "/models", true},                                      // below it
		{"/var/lib/llm-scheduler/data", "/var/lib/llm-scheduler/data", false}, // a bind mount rw
		{"/opt/app", "/", false},                                              // only the overlay root holds it
		{"/modelsX", "/", false},                                              // a prefix of a name is not a parent
		{"/mnt/share/x", "/mnt/share", false},                                 // nested: the longest mount point wins
		{"/mnt/share/inner/x", "/mnt/share/inner", true},
		{"/mnt/share/inner/deeper/x", "/mnt/share/inner/deeper", false},
		{"/mnt/with space/m", "/mnt/with space", false}, // \040 is a space
		{"/mnt/superro/m", "/mnt/superro", true},        // read-only in the super options
		{"/stacked", "/stacked", true},                  // two mounts on one point: the later one is on top
	} {
		ro, mp, ok := MountReadOnly(strings.NewReader(sampleMountinfo), c.dir)
		if !ok || mp != c.mount || ro != c.ro {
			t.Errorf("%s: ro %v mount %q ok %v; want ro %v mount %q", c.dir, ro, mp, ok, c.ro, c.mount)
		}
	}
	// nothing parseable: no answer
	for _, bad := range []string{"", "garbage\n", "1 2 3\n"} {
		if _, _, ok := MountReadOnly(strings.NewReader(bad), "/models"); ok {
			t.Errorf("%q gave an answer", bad)
		}
	}
	// a relative directory gives no answer
	if _, _, ok := MountReadOnly(strings.NewReader(sampleMountinfo), "models"); ok {
		t.Error("a relative directory gave an answer")
	}
}
