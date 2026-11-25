package config

import "testing"

// model_trash_purge (models slice 4) is off unless set: shipping the
// clearing changes nothing until the owner turns it on.
func TestModelTrashPurgeSetting(t *testing.T) {
	c, err := Parse([]byte("profiles: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelTrashPurge || c.ModelTrashDays != DefaultModelTrashDays {
		t.Fatalf("defaults: purge %v days %d", c.ModelTrashPurge, c.ModelTrashDays)
	}
	c, err = Parse([]byte("profiles: {}\nmodel_trash_purge: true\nmodel_trash_days: 7\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.ModelTrashPurge || c.ModelTrashDays != 7 {
		t.Fatalf("set: purge %v days %d", c.ModelTrashPurge, c.ModelTrashDays)
	}
}
