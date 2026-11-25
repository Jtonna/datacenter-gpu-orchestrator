package cfgsvc

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// This version reads stage and display_name and never writes them: a
// profile write that leaves them out keeps the stored ones (a mark a later
// version wrote survives an edit here), one that sets them is refused, a new
// profile cannot carry them. A document import keeps them as written.
func TestProfileMarksAreReadOnly(t *testing.T) {
	ctx := context.Background()
	svc, _ := missingEnv(t, "m.gguf")
	doc := svc.Document()
	p := llama("m.gguf", 4096)
	p.Stage, p.DisplayName = "test", "Mine"
	doc.Profiles["p"] = p
	if _, err := svc.Import(ctx, ui("x"), doc, true); err != nil {
		t.Fatal(err)
	}
	if q := svc.Config().Profiles["p"]; q.Stage != "test" || q.DisplayName != "Mine" {
		t.Fatalf("imported %+v", q)
	}
	// an edit without them keeps them
	if _, err := svc.UpdateProfile(ctx, ui("x"), "p", llama("m.gguf", 8192)); err != nil {
		t.Fatal(err)
	}
	if q := svc.Config().Profiles["p"]; q.Ctx != 8192 || q.Stage != "test" || q.DisplayName != "Mine" {
		t.Fatalf("after edit %+v", q)
	}
	// the same values again pass
	same := llama("m.gguf", 4096)
	same.Stage, same.DisplayName = "test", "Mine"
	if _, err := svc.UpdateProfile(ctx, ui("x"), "p", same); err != nil {
		t.Fatal(err)
	}
	// other values are refused
	other := llama("m.gguf", 4096)
	other.Stage = "production"
	_, err := svc.UpdateProfile(ctx, ui("x"), "p", other)
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Error(), "read only") {
		t.Fatalf("set stage: %v", err)
	}
	created := llama("m.gguf", 4096)
	created.DisplayName = "New"
	if _, err := svc.CreateProfile(ctx, ui("x"), "q", created); !errors.As(err, &ve) {
		t.Fatalf("create with a display name: %v", err)
	}
	// the strict body parser takes the fields (so a read-back body round-trips)
	if b, err := ParseProfile([]byte(`{"model":"m.gguf","ctx":1,"stage":"test","display_name":"x"}`)); err != nil || b.Stage != "test" {
		t.Fatalf("%+v %v", b, err)
	}
}
