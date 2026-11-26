package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The models trash in the UI (v65, models slice 3; boards Models,
// Models-Trash and Models-Delete): the Models list's footer row "Deleted
// models" with the catalog disk's trash figures and the way into the
// Deleted models view (#/models/deleted); that view from GET
// /api/models/trash (each item with its size, profiles, when and by whom,
// an interrupted item with its problem, the repairs of the last start), its
// side panel (?item=<id>) with Restore (a required comment; a 409
// profiles_taken offers rename_taken) and "Remove for good" (a dry run
// first and the server's own sentence, the file name typed exactly and a
// comment, then the remove; refusals in the server's words), the "Removed
// for good" list of the tombstones under the trash; and the panel's
// "Delete model": a dry run first (with
// interrupt when something holds the file, to read would_interrupt), then
// the name typed and a comment, then the delete; interrupt is sent only
// when the dialog showed what it interrupts. Every refusal is a sentence in
// the dialog; read_only is its own state. Nothing is removed automatically
// while the server says purge_on false.

const (
	trashPhi   = "a1b2c3d4e5f6" // phi-4-14b-q4_k_m.gguf, in the trash, 3 profiles
	trashLlama = "f6e5d4c3b2a1" // llama-3.3-70b-q3_k_s.gguf, interrupted
)

// trashRefusal is a refusal of the trash routes in the server's shape (trash.go trashRefuse)
func trashRefusal(status int, reason, msg string, extra map[string]any) map[string]any {
	e := map[string]any{"message": msg, "type": "invalid_request_error", "param": nil, "code": status, "reason": reason}
	for k, v := range extra {
		e[k] = v
	}
	return map[string]any{"$status": status, "$body": map[string]any{"error": e}}
}

// trashDry is a dry run's answer (handleModelDelete's figures)
func trashDry(file string, size int64, profiles []string, would map[string]any) map[string]any {
	f := map[string]any{"file": file, "size_bytes": size, "profiles": profiles, "trash_days": 30, "purge_at": 1797881513419, "purge_on": false, "dry_run": true}
	if would != nil {
		f["would_interrupt"] = would
	}
	return f
}

// trashDone is a delete's answer
func trashDone(file string, size int64, profiles []string, interrupted map[string]any) map[string]any {
	d := map[string]any{"trash_id": "c0ffee100001", "file": file, "size_bytes": size, "deleted_at": 1795289513419, "purge_at": 1797881513419, "trash_days": 30, "purge_on": false,
		"profiles_removed": profiles, "revision": 53, "warnings": []any{}, "audit_id": 301}
	if interrupted != nil {
		d["interrupted"] = interrupted
	}
	return d
}

// The request lines a delete sends ("$match" in ui-exec picks the answer by them)
const (
	reDryPlain     = `^POST /api/models/delete/[^ ?]+\?dry_run=1 \{"comment":"[^"]+","dry_run":true\}$`
	reDryInterrupt = `^POST /api/models/delete/[^ ?]+\?dry_run=1 \{"comment":"[^"]+","dry_run":true,"interrupt":true\}$`
	reRealPlain    = `^POST /api/models/delete/[^ ?]+ \{"comment":"[^"]+"\}$`
	reRealInt      = `^POST /api/models/delete/[^ ?]+ \{"comment":"[^"]+","interrupt":true\}$`
	reAnyReal      = `^POST /api/models/delete/[^ ?]+ `
)

func TestUINextModelsTrash(t *testing.T) {
	paths := []string{"/api/models", "/api/models/trash", "/api/status"}
	two := fixtureAnswers(t, []string{"--trash", "two"}, paths...)
	empty := fixtureAnswers(t, []string{"--trash", "empty"}, paths...)
	st := two["/api/status"]
	with := func(base map[string]any, extra map[string]any) map[string]any {
		f := map[string]any{}
		for k, v := range base {
			f[k] = v
		}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	run := func(f map[string]any, cases ...any) {
		t.Helper()
		uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": cases})
	}
	comment := func(text string) map[string]any {
		return map[string]any{"type": "div.dlg textarea.dlg-comment", "value": text}
	}
	confirm := func(text string) map[string]any {
		return map[string]any{"type": "div.dlg input[data-confirm]", "value": text}
	}

	// 1. the footer row and the view, with nothing in the trash
	run(empty,
		map[string]any{"name": "trash: the footer row, nothing deleted", "route": "#/models", "status": st,
			"expect": []string{"No deleted models", "View"},
			"has":    []string{"a.tbar[data-act=trash][href='#/models/deleted']"},
			"none":   []string{"button.tbar[data-off]", ".tbar [data-off]"}, "absent": []string{"not built yet · Deleted", "Deleted models · not built yet"}},
		map[string]any{"name": "trash: the view, empty", "route": "#/models/deleted", "status": st,
			"expect": []string{"All models", "Nothing in deleted models", "A model you delete waits here"},
			"has":    []string{"a.btn[href='#/models']"}, "none": []string{"div.mr.del[data-trash]"}},
	)

	// 2. two items, one interrupted; the repairs of the last start
	run(two,
		map[string]any{"name": "trash: the footer row with the trash figures", "route": "#/models", "status": st,
			"expect": []string{"34.6 GB in deleted models", "2 files", "nothing is removed automatically yet", "View"},
			"absent": []string{"clears in", "not built yet · ", "Deleted models · not built yet"},
			"has":    []string{"a.tbar[data-act=trash][href='#/models/deleted']"}},
		map[string]any{"name": "trash: the view lists the items", "route": "#/models/deleted", "status": st,
			"expect": []string{"All models", "Model", "Profiles", "Deleted", "Kept until", "phi-4-14b-q4_k_m.gguf", "8.4 GB", "3 profiles", "3 days ago",
				"llama-3.3-70b-q3_k_s.gguf", "26.2 GB", "1 profile", "28 days ago", "Needs a look", "not in exactly one place",
				"At the last start", "needs a look", "nothing is removed automatically yet"},
			"has": []string{"div.mr.del[data-trash='" + trashPhi + "']", "div.mr.del[data-trash='" + trashLlama + "'][data-state=interrupted]",
				"div.banner[data-repairs]", "label.sx input", "select.fs[aria-label=Sort]"},
			"absent": []string{"clears in", "Clears in"}},
		map[string]any{"name": "trash: search", "route": "#/models/deleted?q=llama", "status": st, "expect": []string{"llama-3.3-70b-q3_k_s.gguf"}, "absent": []string{"phi-4-14b-q4_k_m.gguf"}},
		map[string]any{"name": "trash: a row opens its panel", "route": "#/models/deleted", "status": st, "click": []string{"div.mr.del[data-trash='" + trashPhi + "']"},
			"hash": "#/models/deleted?item=" + trashPhi},
		map[string]any{"name": "trash: the panel of an item", "route": "#/models/deleted?item=" + trashPhi, "status": st,
			"page": []string{"Deleted model", "phi-4-14b-q4_k_m.gguf", "8.4 GB", "deleted", "by owner", "not used since qwen3-14b arrived", "Kept until",
				"Restore model", "Remove for good", "Configurations", "3 profiles", "phi-4-14b:16k", "16,384", "10,350 MiB", "10 min", "5 min"},
			"pageHas": []string{"aside.side button[data-act=restore]", "aside.side button[data-act=remove]",
				"aside.side tr[data-profile='phi-4-14b:json']"},
			"pageNone": []string{"aside.side button[data-act=restore][data-off]", "aside.side button[data-act=remove][data-off]"},
			"has":      []string{"div.mr.del.sel[data-trash='" + trashPhi + "']"}},
		map[string]any{"name": "trash: an interrupted item cannot be restored or removed", "route": "#/models/deleted?item=" + trashLlama, "status": st,
			"page":    []string{"llama-3.3-70b-q3_k_s.gguf", "Needs a look", "not in exactly one place"},
			"pageHas": []string{"aside.side button[data-act=restore][data-off][aria-disabled=true]", "aside.side button[data-act=remove][data-off][aria-disabled=true]"}},
		map[string]any{"name": "trash: Removed for good under the trash", "route": "#/models/deleted", "status": st,
			"expect": []string{"Removed for good", "2 files", "mistral-7b-v0.3-q4_k_m.gguf", "4.4 GB · 2 profiles", "2 days ago", "owner", "removed for good: the disk was full before the 72B arrived",
				"tinyllama-1.1b-q8_0.gguf", "15 days ago", "potluck", "the timed clearing (model_trash_days)"},
			"has":   []string{"section.del-gone div.mr.del.gone[data-removed='0a0b0c0d0e0f'][data-reason=owner]", "section.del-gone div.mr.del.gone[data-removed='1a1b1c1d1e1f'][data-reason=clearing]"},
			"order": []string{"mistral-7b-v0.3-q4_k_m.gguf", "tinyllama-1.1b-q8_0.gguf"}},
	)
	// a remove stamped by the server a moment ahead of this clock reads "just now", never "in 0 s"
	ahead := map[string]any{}
	if err := json.Unmarshal([]byte(mustJSON(t, two["/api/models/trash"])), &ahead); err != nil {
		t.Fatal(err)
	}
	ahead["removed"].([]any)[0].(map[string]any)["removed_at"] = float64(time.Now().Add(time.Hour).UnixMilli())
	ahead["removed"].([]any)[0].(map[string]any)["size_bytes"] = float64(52_400_000) // under 0.1 GB: in MB
	run(with(two, map[string]any{"/api/models/trash": ahead}),
		map[string]any{"name": "trash: a remove stamped ahead of the clock", "route": "#/models/deleted", "status": st,
			"expect": []string{"just now", "52 MB · 2 profiles"}, "absent": []string{"in 0 s", "in 1 h", "in 59 min", "0.1 GB ·", "0.0 GB"}})

	// 2b. remove for good: the dry run first and the server's sentence; the
	// file name typed exactly and a comment; then the remove; the refusals
	removeURL := "/api/models/trash/" + trashPhi + "/remove"
	sentence := "This cannot be undone. phi-4-14b-q4_k_m.gguf and its 3 profiles are erased now, 27 days before they would have been. 8.4 GB is freed."
	rmDry := map[string]any{"file": "phi-4-14b-q4_k_m.gguf", "trash_id": trashPhi, "size_bytes": 8400000000, "profiles": []any{"phi-4-14b", "phi-4-14b:16k", "phi-4-14b:json"},
		"deleted_at": 1795289513419, "purge_at": 1797881513419, "days_early": 27, "cannot_undo": true, "purge_on": false, "message": sentence, "dry_run": true, "read_only": false}
	rmDone := map[string]any{"file": "phi-4-14b-q4_k_m.gguf", "trash_id": trashPhi, "removed_at": 1795589513419, "freed_bytes": 8400000000,
		"profiles_removed": []any{"phi-4-14b", "phi-4-14b:16k", "phi-4-14b:json"}, "reason": "owner", "cannot_undo": true, "audit_id": 501}
	const reRmDry, reRmReal = `^POST /api/models/trash/[^/]+/remove\?dry_run=1 \{"comment":"[^"]+","dry_run":true\}$`, `^POST /api/models/trash/[^/]+/remove \{"comment":"[^"]+"\}$`
	rmOpen := "aside.side button[data-act=remove]"
	roDry := map[string]any{}
	for k, v := range rmDry {
		roDry[k] = v
	}
	roDry["read_only"] = true
	notRegular := "phi-4-14b-q4_k_m.gguf was not removed: .trash/a1b2c3d4e5f6/phi-4-14b-q4_k_m.gguf is a symbolic link. Potluck erases only a plain file in a plain trash folder; nothing was changed"
	rmRoMsg := "the models folder is read-only here: nothing was changed (unlink /models/.trash/a1b2c3d4e5f6/phi-4-14b-q4_k_m.gguf: read-only file system). Deleting and restoring model files need the scheduler's models folder mounted read-write."
	run(with(two, map[string]any{removeURL: map[string]any{"$match": []any{[]any{reRmDry, rmDry}, []any{reRmReal, rmDone}}}}),
		map[string]any{"name": "remove: the dry run first, the server's sentence", "route": "#/models/deleted?item=" + trashPhi, "status": st, "click": []string{rmOpen},
			"page":         []string{"Remove phi-4-14b-q4_k_m.gguf for good?", sentence, "Its profiles go with it: phi-4-14b, phi-4-14b:16k and phi-4-14b:json.", "Type phi-4-14b-q4_k_m.gguf to confirm", "Comment (required)"},
			"pageHas":      []string{"div.dlg[data-dlg=remove][data-state=ready] [data-cannot-undo] [data-msg]", "div.dlg button[data-go][disabled]"},
			"fetchedQuery": []string{reRmDry}, "notFetched": []string{reRmReal}},
		map[string]any{"name": "remove: a name not typed exactly keeps it off", "route": "#/models/deleted?item=" + trashPhi, "status": st,
			"click":   []any{rmOpen, confirm("phi-4-14b-q4_k_m.GGUF"), comment("free the disk"), "div.dlg button[data-go]"},
			"pageHas": []string{"div.dlg button[data-go][disabled]"}, "notFetched": []string{reRmReal}},
		map[string]any{"name": "remove: no comment keeps it off", "route": "#/models/deleted?item=" + trashPhi, "status": st,
			"click":   []any{rmOpen, confirm("phi-4-14b-q4_k_m.gguf"), "div.dlg button[data-go]"},
			"pageHas": []string{"div.dlg button[data-go][disabled]"}, "notFetched": []string{reRmReal}},
		map[string]any{"name": "remove: typed and commented, it goes", "route": "#/models/deleted?item=" + trashPhi, "status": st,
			"click":        []any{rmOpen, confirm("phi-4-14b-q4_k_m.gguf"), comment("free the disk"), "div.dlg button[data-go]"},
			"fetchedQuery": []string{reRmDry, `^POST /api/models/trash/` + trashPhi + `/remove \{"comment":"free the disk"\}$`}, "sentHeader": []string{"^X-Comment: free the disk$"},
			"expect": []string{"Removed phi-4-14b-q4_k_m.gguf for good: 8.4 GB freed; 3 profiles went with it."}, "pageNone": []string{"div.dlg"}, "hash": "#/models/deleted"},
	)
	// while the dry run is on its way: "checking", nothing to type, only Cancel
	run(with(two, map[string]any{removeURL: map[string]any{"$match": []any{[]any{reRmDry, map[string]any{"$hang": true}}}}}),
		map[string]any{"name": "remove: checking while the dry run is on its way", "route": "#/models/deleted?item=" + trashPhi, "status": st, "click": []string{rmOpen},
			"page":    []string{"Remove phi-4-14b-q4_k_m.gguf for good?", "Checking what removing it would change…", "Cancel"},
			"pageHas": []string{"div.dlg[data-dlg=remove][data-state=checking] p[role=status]", "div.dlg button[data-close]"},
			"pageNone": []string{"div.dlg input[data-confirm]", "div.dlg textarea", "div.dlg button[data-go]", "div.dlg [data-msg]"},
			"fetchedQuery": []string{reRmDry}, "notFetched": []string{reRmReal}})
	run(with(two, map[string]any{removeURL: map[string]any{"$match": []any{[]any{reRmDry, roDry}, []any{reRmReal, trashRefusal(409, "read_only", rmRoMsg, nil)}}}}),
		map[string]any{"name": "remove: the dry run says read-only", "route": "#/models/deleted?item=" + trashPhi, "status": st, "click": []string{rmOpen},
			"page":    []string{sentence, "The models folder is read-only on this scheduler: removing a file is not possible here. Nothing can be changed."},
			"pageHas": []string{"div.dlg[data-dlg=remove][data-state=read-only] [data-ro-flag]"}, "pageNone": []string{"div.dlg input[data-confirm]", "div.dlg textarea"},
			"notFetched": []string{reRmReal}})
	run(with(two, map[string]any{removeURL: map[string]any{"$match": []any{[]any{reRmDry, rmDry}, []any{reRmReal, trashRefusal(409, "read_only", rmRoMsg, nil)}}}}),
		map[string]any{"name": "remove: the server refuses read_only", "route": "#/models/deleted?item=" + trashPhi, "status": st,
			"click":   []any{rmOpen, confirm("phi-4-14b-q4_k_m.gguf"), comment("free the disk"), "div.dlg button[data-go]"},
			"page":    []string{"The models folder is read-only on this scheduler: nothing was changed.", rmRoMsg, "phi-4-14b-q4_k_m.gguf stays in Deleted models."},
			"pageHas": []string{"div.dlg[data-dlg=remove][data-state=read-only] [data-ro]"}, "notOnPage": []string{"Removed phi-4-14b-q4_k_m.gguf for good"}})
	run(with(two, map[string]any{removeURL: trashRefusal(409, "not_regular", notRegular, nil)}),
		map[string]any{"name": "remove: not a plain file, in the server's words", "route": "#/models/deleted?item=" + trashPhi, "status": st, "click": []string{rmOpen},
			"page":    []string{"Not removed: it is not a plain file in a plain trash folder.", "Nothing was erased.", notRegular},
			"pageHas": []string{"div.dlg[data-dlg=remove][data-state=refused] [data-refused=not_regular]"}, "pageNone": []string{"div.dlg input[data-confirm]"}})

	// 3. restore: the comment is required; it works; the view and the footer are read again
	restoreURL := "/api/models/trash/" + trashPhi + "/restore"
	restored := map[string]any{"file": "phi-4-14b-q4_k_m.gguf", "trash_id": trashPhi, "profiles_restored": []any{"phi-4-14b", "phi-4-14b:16k", "phi-4-14b:json"},
		"renamed": map[string]any{}, "aliases_dropped": []any{}, "revision": 61, "audit_id": 401}
	run(with(two, map[string]any{"POST " + restoreURL: restored}),
		map[string]any{"name": "trash: restore needs a comment", "route": "#/models/deleted?item=" + trashPhi, "status": st,
			"click":   []any{"aside.side button[data-act=restore]", "div.dlg button[data-go]"},
			"pageHas": []string{"div.dlg[data-dlg=restore] button[data-go][disabled]"}, "page": []string{"3 profiles come back", "phi-4-14b:16k"},
			"notFetched": []string{"^POST /api/models/trash/"}},
		map[string]any{"name": "trash: restore with a comment", "route": "#/models/deleted?item=" + trashPhi, "status": st,
			"click":        []any{"aside.side button[data-act=restore]", comment("needed again"), "div.dlg button[data-go]"},
			"fetchedQuery": []string{`^POST /api/models/trash/` + trashPhi + `/restore \{"comment":"needed again"\}$`},
			"sentHeader":   []string{`^X-Comment: needed again$`},
			"page":         []string{"Restored phi-4-14b-q4_k_m.gguf", "3 profiles are back"}, "pageNone": []string{"div.dlg"}},
	)

	// 4. restore refused profiles_taken: the dialog offers rename_taken as the server describes it
	takenMsg := "these names of phi-4-14b-q4_k_m.gguf's profiles are taken now: profile phi-4-14b. Restore with rename_taken: true to add them as <id>-restored (taken aliases are left off)"
	renamedOK := with(restored, map[string]any{"renamed": map[string]any{"phi-4-14b": "phi-4-14b-restored"}, "profiles_restored": []any{"phi-4-14b-restored", "phi-4-14b:16k", "phi-4-14b:json"}})
	taken := with(two, map[string]any{"POST " + restoreURL: map[string]any{"$match": []any{
		[]any{`"rename_taken":true`, renamedOK},
		[]any{`.`, trashRefusal(409, "profiles_taken", takenMsg, map[string]any{"taken": []any{"profile phi-4-14b"}})}}}})
	run(taken,
		map[string]any{"name": "trash: restore refused, names taken", "route": "#/models/deleted?item=" + trashPhi, "status": st,
			"click":     []any{"aside.side button[data-act=restore]", comment("back"), "div.dlg button[data-go]"},
			"page":      []string{"Some names of its profiles are taken now", "profile phi-4-14b", takenMsg, "Restore with new names", "-restored"},
			"pageHas":   []string{"div.dlg[data-dlg=restore][data-state=taken] button[data-go=rename]"},
			"notOnPage": []string{`"reason"`, `{"error"`}},
		map[string]any{"name": "trash: restore with new names", "route": "#/models/deleted?item=" + trashPhi, "status": st,
			"click":        []any{"aside.side button[data-act=restore]", comment("back"), "div.dlg button[data-go]", "div.dlg button[data-go=rename]"},
			"fetchedQuery": []string{`^POST /api/models/trash/` + trashPhi + `/restore \{"comment":"back","rename_taken":true\}$`},
			"page":         []string{"Restored phi-4-14b-q4_k_m.gguf", "phi-4-14b is back as phi-4-14b-restored"}},
	)

	// 5. delete: the dry run, the name typed, the comment, the delete
	qwen4 := mdlNever // qwen3-4b-Q8_0.gguf: nothing holds it
	okDel := with(two, map[string]any{"POST /api/models/delete/" + qwen4: map[string]any{"$match": []any{
		[]any{reDryPlain, trashDry(qwen4, 4300000000, []string{"qwen3-4b"}, nil)},
		[]any{reRealPlain, trashDone(qwen4, 4300000000, []string{"qwen3-4b"}, nil)}}}})
	open := []any{"aside.side button[data-act=delete]"}
	run(okDel,
		map[string]any{"name": "delete: the dialog after its dry run", "route": "#/models?model=" + qwen4, "status": st, "click": open,
			"fetchedQuery": []string{`^POST /api/models/delete/qwen3-4b-Q8_0\.gguf\?dry_run=1 \{"comment":"[^"]+","dry_run":true\}$`},
			"notFetched":   []string{reAnyReal, reDryInterrupt},
			"page":         []string{"Delete qwen3-4b-Q8_0.gguf?", "Kept in Deleted models", "4.3 GB", "Restorable", "1 profile", "Type", "qwen3-4b", "to confirm", "Nothing is interrupted"},
			"pageHas":      []string{"div.dlg[data-dlg=delete][data-state=ready] input[data-confirm]", "div.dlg textarea.dlg-comment", "div.dlg button[data-go][disabled]"},
			"notOnPage":    []string{"This interrupts", "clears in"}},
		map[string]any{"name": "delete: the name not typed, nothing sent", "route": "#/models?model=" + qwen4, "status": st,
			"click":      append(append([]any{}, open...), comment("gone"), confirm("qwen3"), "div.dlg button[data-go]"),
			"notFetched": []string{reAnyReal}, "pageHas": []string{"div.dlg button[data-go][disabled]"}},
		map[string]any{"name": "delete: no comment, nothing sent", "route": "#/models?model=" + qwen4, "status": st,
			"click":      append(append([]any{}, open...), confirm("qwen3-4b"), "div.dlg button[data-go]"),
			"notFetched": []string{reAnyReal}, "pageHas": []string{"div.dlg button[data-go][disabled]"}},
		map[string]any{"name": "delete: it works", "route": "#/models?model=" + qwen4, "status": st,
			"click":        append(append([]any{}, open...), confirm("qwen3-4b"), comment("not used"), "div.dlg button[data-go]"),
			"fetchedQuery": []string{`^POST /api/models/delete/qwen3-4b-Q8_0\.gguf \{"comment":"not used"\}$`, `^GET /api/models/trash$|^GET /api/models$`},
			"sentHeader":   []string{`^X-Comment: not used$`},
			"expect":       []string{"Deleted qwen3-4b-Q8_0.gguf", "It is in Deleted models", "1 profile left the configuration"},
			"pageNone":     []string{"div.dlg", "aside.side"}, "hash": "#/models"},
	)

	// 6. delete refused in_use (instances and queued): the dry run with interrupt says what it interrupts; the owner confirms; interrupt is sent
	gemma := mdlLoaded
	would := map[string]any{"instances": []any{map[string]any{"profile": "gemma-4-27b", "state": "ready", "in_flight": 2}}, "queued": []any{map[string]any{"profile": "gemma-4-27b:long", "waiters": 1}}}
	inUseMsg := gemma + " is in use and was not deleted: gemma-4-27b is ready; 1 request(s) for gemma-4-27b:long are queued. Unload it (or wait for the runs) first, or delete with interrupt."
	gemmaProfiles := []string{"bench-gemma-4-27b-c16k-n2", "gemma-4-27b", "gemma-4-27b:long"}
	inUse := func(real any) map[string]any {
		return with(two, map[string]any{"POST /api/models/delete/" + gemma: map[string]any{"$match": []any{
			[]any{reDryInterrupt, trashDry(gemma, 16500000000, gemmaProfiles, would)},
			[]any{reDryPlain, trashRefusal(409, "in_use", inUseMsg, would)},
			[]any{reRealInt, real}}}})
	}
	interrupted := map[string]any{"instances": []any{map[string]any{"profile": "gemma-4-27b", "state": "ready", "in_flight": 2, "waiters": 0}}, "requests_cut": 2, "queued_cancelled": 1}
	run(inUse(trashDone(gemma, 16500000000, gemmaProfiles, interrupted)),
		map[string]any{"name": "delete: in use, the dialog says what it interrupts", "route": "#/models?model=" + gemma, "status": st, "click": open,
			"fetchedQuery": []string{reDryPlain, reDryInterrupt}, "notFetched": []string{reAnyReal},
			"page": []string{"This interrupts 3 requests.", "gemma-4-27b is loaded on ps5-01 and ps5-02 with 2 requests in flight", "1 request for gemma-4-27b:long is queued",
				"unloaded", "Kept in Deleted models", "16.5 GB", "2 profiles", "1 test profile"},
			"pageHas": []string{"div.dlg[data-dlg=delete][data-state=ready] [data-interrupts]", "div.dlg input[data-confirm]"}},
		map[string]any{"name": "delete: in use, confirmed with interrupt", "route": "#/models?model=" + gemma, "status": st,
			"click":        append(append([]any{}, open...), confirm("gemma-4-27b"), comment("replace it"), "div.dlg button[data-go]"),
			"fetchedQuery": []string{`^POST /api/models/delete/gemma-4-27b-it-Q4_K_M\.gguf \{"comment":"replace it","interrupt":true\}$`},
			"expect":       []string{"Deleted gemma-4-27b-it-Q4_K_M.gguf", "2 requests were cut", "1 queued request was cancelled"}},
	)

	// 6b. zone names in the answer (v67): used as they are, the catalog's
	// zones (ps5-01, ps5-02) are not joined in; without them (an older
	// server) the catalog join above is the fallback
	wouldZ := map[string]any{"instances": []any{map[string]any{"profile": "gemma-4-27b", "state": "ready", "in_flight": 2, "nodes": []any{"zone-07", "zone-08"}}},
		"queued": []any{map[string]any{"profile": "gemma-4-27b:long", "waiters": 1}}}
	run(with(two, map[string]any{"POST /api/models/delete/" + gemma: map[string]any{"$match": []any{
		[]any{reDryInterrupt, trashDry(gemma, 16500000000, gemmaProfiles, wouldZ)},
		[]any{reDryPlain, trashRefusal(409, "in_use", inUseMsg, wouldZ)}}}}),
		map[string]any{"name": "delete: in use, zone names from the answer", "route": "#/models?model=" + gemma, "status": st, "click": open,
			"page":      []string{"This interrupts 3 requests.", "gemma-4-27b is loaded on zone-07 and zone-08 with 2 requests in flight"},
			"notOnPage": []string{"loaded on ps5-01", "ps5-02 with"}, "pageHas": []string{"div.dlg [data-interrupts] .mono"}})

	// 6c. something held it, then nothing: the dry run with interrupt answers
	// 200 with no would_interrupt key. Nothing is interrupted, and the
	// delete is sent without interrupt.
	run(with(two, map[string]any{"POST /api/models/delete/" + gemma: map[string]any{"$match": []any{
		[]any{reDryInterrupt, trashDry(gemma, 16500000000, gemmaProfiles, nil)},
		[]any{reDryPlain, trashRefusal(409, "in_use", inUseMsg, would)},
		[]any{reRealPlain, trashDone(gemma, 16500000000, gemmaProfiles, nil)}}}}),
		map[string]any{"name": "delete: freed meanwhile, no would_interrupt", "route": "#/models?model=" + gemma, "status": st, "click": open,
			"fetchedQuery": []string{reDryPlain, reDryInterrupt}, "page": []string{"Nothing is interrupted"},
			"pageHas": []string{"div.dlg[data-dlg=delete][data-state=ready] [data-free]"}, "pageNone": []string{"[data-interrupts]"}},
		map[string]any{"name": "delete: freed meanwhile, sent without interrupt", "route": "#/models?model=" + gemma, "status": st,
			"click":        append(append([]any{}, open...), confirm("gemma-4-27b"), comment("replace it"), "div.dlg button[data-go]"),
			"fetchedQuery": []string{`^POST /api/models/delete/gemma-4-27b-it-Q4_K_M\.gguf \{"comment":"replace it"\}$`},
			"notFetched":   []string{reRealInt}, "expect": []string{"Deleted gemma-4-27b-it-Q4_K_M.gguf"}})

	// 7. interrupt_incomplete: nothing moved; the dialog says so and can check again
	stillMsg := gemma + " was not deleted: after the interrupt, within 5s, still there: gemma-4-27b is stopping. Nothing was moved; the profiles are unchanged."
	run(inUse(trashRefusal(409, "interrupt_incomplete", stillMsg, map[string]any{"interrupted": interrupted, "still": []any{"gemma-4-27b is stopping"}})),
		map[string]any{"name": "delete: the interrupt did not finish", "route": "#/models?model=" + gemma, "status": st,
			"click":   append(append([]any{}, open...), confirm("gemma-4-27b"), comment("replace it"), "div.dlg button[data-go]"),
			"page":    []string{"Not deleted: the interrupt did not finish in time", stillMsg, "Check again"},
			"pageHas": []string{"div.dlg[data-dlg=delete][data-state=refused] button[data-go=again]"}, "notOnPage": []string{`{"error"`}},
	)

	// 8. a benchmark run uses it: the dialog can only be closed
	qwen14 := mdlRecent
	runMsg := qwen14 + " is in use and was not deleted: benchmark run(s) r-12 use it. Unload it (or wait for the runs) first, or delete with interrupt."
	run(with(two, map[string]any{"POST /api/models/delete/" + qwen14: map[string]any{"$match": []any{
		[]any{`.`, trashRefusal(409, "in_use", runMsg, map[string]any{"runs": []any{"r-12"}})}}}}),
		map[string]any{"name": "delete: blocked by a benchmark run", "route": "#/models?model=" + qwen14, "status": st, "click": open,
			"page":       []string{"A benchmark run uses this model", "r-12", "Cancel the run or wait for it", runMsg, "Close"},
			"pageHas":    []string{"div.dlg[data-dlg=delete][data-state=blocked]", "div.dlg a[href='#/benchmarks/runs/r-12']"},
			"pageNone":   []string{"div.dlg input[data-confirm]", "div.dlg button[data-go]", "div.dlg textarea"},
			"notFetched": []string{reAnyReal, reDryInterrupt}},
	)

	// 9. read_only: its own state; nothing in the list changes; the next dialog says so before the name is typed
	roMsg := "the models folder is read-only here: nothing was changed (mkdir /models/.trash/c0ffee100001: read-only file system). Deleting and restoring model files need the scheduler's models folder mounted read-write."
	ro := with(two, map[string]any{
		"POST /api/models/delete/" + qwen4: map[string]any{"$match": []any{
			[]any{reDryPlain, trashDry(qwen4, 4300000000, []string{"qwen3-4b"}, nil)},
			[]any{reRealPlain, trashRefusal(409, "read_only", roMsg, nil)}}},
		"POST " + restoreURL: trashRefusal(409, "read_only", "the models folder is read-only here: nothing was changed (rename /models/.trash/"+trashPhi+"/phi-4-14b-q4_k_m.gguf /models/phi-4-14b-q4_k_m.gguf: read-only file system). Deleting and restoring model files need the scheduler's models folder mounted read-write.", nil)})
	run(ro,
		map[string]any{"name": "delete: the models folder is read-only", "route": "#/models?model=" + qwen4, "status": st,
			"click":   append(append([]any{}, open...), confirm("qwen3-4b"), comment("tidy"), "div.dlg button[data-go]"),
			"page":    []string{"The models folder is read-only on this scheduler: nothing was changed", roMsg},
			"pageHas": []string{"div.dlg[data-dlg=delete][data-state=read-only]"}, "notOnPage": []string{"Deleted qwen3-4b-Q8_0.gguf", `{"error"`},
			"has": []string{"div.mr[data-model='" + qwen4 + "']"}},
		map[string]any{"name": "delete: read-only is said before the name is typed next time", "route": "#/models?model=" + qwen4, "status": st,
			"click":   append(append(append([]any{}, open...), confirm("qwen3-4b"), comment("tidy"), "div.dlg button[data-go]", "div.dlg button[data-close]"), open...),
			"pageHas": []string{"div.dlg[data-dlg=delete][data-state=ready] [data-ro-note]"}, "page": []string{"answered that its models folder is read-only"}},
		map[string]any{"name": "restore: the models folder is read-only", "route": "#/models/deleted?item=" + trashPhi, "status": st,
			"click":   []any{"aside.side button[data-act=restore]", comment("back"), "div.dlg button[data-go]"},
			"page":    []string{"The models folder is read-only on this scheduler: nothing was changed", "rename /models/.trash/"},
			"pageHas": []string{"div.dlg[data-dlg=restore][data-state=read-only]", "div.mr.del[data-trash='" + trashPhi + "']"}, "notOnPage": []string{"Restored phi"}},
	)

	// 10. loaded_again: the revision was undone, nothing moved
	againMsg := qwen4 + " was not deleted: it was loaded again meanwhile (qwen3-4b is starting); the profiles were put back and nothing was moved"
	run(with(two, map[string]any{"POST /api/models/delete/" + qwen4: map[string]any{"$match": []any{
		[]any{reDryPlain, trashDry(qwen4, 4300000000, []string{"qwen3-4b"}, nil)},
		[]any{reRealPlain, trashRefusal(409, "loaded_again", againMsg, nil)}}}}),
		map[string]any{"name": "delete: loaded again meanwhile", "route": "#/models?model=" + qwen4, "status": st,
			"click":   append(append([]any{}, open...), confirm("qwen3-4b"), comment("tidy"), "div.dlg button[data-go]"),
			"page":    []string{"Not deleted: it was loaded again meanwhile", againMsg, "Check again"},
			"pageHas": []string{"div.dlg[data-dlg=delete][data-state=refused]"}},
	)

	// 11. models_writable (catalog disk and trash list): false opens the
	// read-only state at once, with no name, no comment and nothing sent;
	// true or absent: the dialogs as above
	roFx := fixtureAnswers(t, []string{"--trash", "read-only"}, paths...)
	flagOf := func(f map[string]any) (disk, trash any) {
		return f["/api/models"].(map[string]any)["disk"].(map[string]any)["models_writable"], f["/api/models/trash"].(map[string]any)["models_writable"]
	}
	if d, tr := flagOf(roFx); d != false || tr != false {
		t.Fatalf("fixture --trash read-only: models_writable disk %v, trash %v; want false", d, tr)
	}
	if d, tr := flagOf(two); d != true || tr != true {
		t.Fatalf("fixture --trash two: models_writable disk %v, trash %v; want true", d, tr)
	}
	roText := "The models folder is read-only on this scheduler: deleting and restoring are not possible here. Nothing can be changed."
	run(roFx,
		map[string]any{"name": "flag false: delete opens the read-only state", "route": "#/models?model=" + qwen4, "status": st, "click": open,
			"page":       []string{"Delete qwen3-4b-Q8_0.gguf?", roText, "4.3 GB", "1 profile", "Close"},
			"pageHas":    []string{"div.dlg[data-dlg=delete][data-state=read-only] [data-ro-flag]", "div.dlg button[data-close]"},
			"pageNone":   []string{"div.dlg input[data-confirm]", "div.dlg textarea", "div.dlg button[data-go]"},
			"notFetched": []string{"^POST "}},
		map[string]any{"name": "flag false: Delete model stays visible", "route": "#/models?model=" + qwen4, "status": st,
			"pageHas": []string{"aside.side button[data-act=delete]"}, "pageNone": []string{"aside.side button[data-act=delete][data-off]"}},
		map[string]any{"name": "flag false: Close closes it", "route": "#/models?model=" + qwen4, "status": st, "click": []any{"aside.side button[data-act=delete]", "div.dlg button[data-close]"},
			"pageNone": []string{"div.dlg"}, "notFetched": []string{"^POST "}},
		map[string]any{"name": "flag false: restore opens the read-only state", "route": "#/models/deleted?item=" + trashPhi, "status": st,
			"click":      []any{"aside.side button[data-act=restore]"},
			"page":       []string{"Restore phi-4-14b-q4_k_m.gguf?", roText, "8.4 GB", "3 profiles"},
			"pageHas":    []string{"div.dlg[data-dlg=restore][data-state=read-only] [data-ro-flag]", "aside.side button[data-act=restore]"},
			"pageNone":   []string{"div.dlg textarea", "div.dlg button[data-go]", "aside.side button[data-act=restore][data-off]"},
			"notFetched": []string{"^POST "}},
	)
	run(okDel,
		map[string]any{"name": "flag true: the delete dialog as before", "route": "#/models?model=" + qwen4, "status": st, "click": open,
			"pageHas": []string{"div.dlg[data-dlg=delete][data-state=ready] input[data-confirm]"}, "pageNone": []string{"[data-ro-flag]"}, "notOnPage": []string{roText}},
		map[string]any{"name": "flag true: the restore dialog as before", "route": "#/models/deleted?item=" + trashPhi, "status": st, "click": []any{"aside.side button[data-act=restore]"},
			"pageHas": []string{"div.dlg[data-dlg=restore][data-state=ask] textarea.dlg-comment"}, "pageNone": []string{"[data-ro-flag]"}},
	)
	noFlag := with(okDel, map[string]any{"/api/models": withoutFlag(okDel["/api/models"], "disk"), "/api/models/trash": withoutFlag(okDel["/api/models/trash"], "")})
	if d, tr := flagOf(noFlag); d != nil || tr != nil {
		t.Fatalf("withoutFlag left the field: %v %v", d, tr)
	}
	run(noFlag,
		map[string]any{"name": "flag absent: the delete dialog as before", "route": "#/models?model=" + qwen4, "status": st, "click": open,
			"pageHas": []string{"div.dlg[data-dlg=delete][data-state=ready] input[data-confirm]"}, "pageNone": []string{"[data-ro-flag]"}},
		map[string]any{"name": "flag absent: the restore dialog as before", "route": "#/models/deleted?item=" + trashPhi, "status": st, "click": []any{"aside.side button[data-act=restore]"},
			"pageHas": []string{"div.dlg[data-dlg=restore][data-state=ask] textarea.dlg-comment"}, "pageNone": []string{"[data-ro-flag]"}},
	)
}

// withoutFlag is a copy of a catalog (in "disk") or trash list (in "")
// answer without models_writable, as an older server or one that cannot tell
func withoutFlag(doc any, in string) map[string]any {
	src := doc.(map[string]any)
	out := map[string]any{}
	for k, v := range src {
		out[k] = v
	}
	if in == "" {
		delete(out, "models_writable")
		return out
	}
	inner := map[string]any{}
	for k, v := range src[in].(map[string]any) {
		inner[k] = v
	}
	delete(inner, "models_writable")
	out[in] = inner
	return out
}

// The same screens on a real scheduler with a throwaway models folder
// (trashEnv): the footer row and the view from the server's own answers, a
// delete from the panel (the dry run, then the delete) and a restore from
// the view go through to the server and move the files; then, with the
// folder read-only, the delete dialog shows the server's read_only refusal
// as its own state and nothing moves.
func TestUINextModelsTrashOnARealServer(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"not used any more"}`, trashUI)
	if st != 200 {
		t.Fatalf("delete a.gguf: %d %v", st, m)
	}
	id := m["trash_id"].(string)
	short := ""
	for k, x := range catalogKeys(t, te.env) {
		if k == "b.gguf" {
			short, _ = x["short_name"].(string)
		}
	}
	if short == "" {
		short = "b.gguf"
	}
	run := func(cases ...any) {
		t.Helper()
		uiExec(t, map[string]any{"shell": "next", "app": true, "base": te.front.URL,
			"passWrites": []string{`^POST /api/models/delete/b\.gguf `, `^POST /api/models/delete/a\.gguf `, `^POST /api/models/trash/` + id + `/restore `}, "cases": cases})
	}
	comment := func(text string) map[string]any {
		return map[string]any{"type": "div.dlg textarea.dlg-comment", "value": text}
	}
	run(
		map[string]any{"name": "real: the footer row", "route": "#/models", "status": "fetch",
			// the test's files are a few bytes: "under 1 MB", never "0.0 GB" (the reviewer, 26 Nov)
			"expect": []string{"under 1 MB in deleted models", "1 file", "nothing is removed automatically yet", "View"}, "has": []string{"a.tbar[data-act=trash]"},
			// the list's rows write a tiny file's size the same way as the trash: "under 1 MB", never "0.0 GB"
			"absent": []string{"0.0 GB"}},
		map[string]any{"name": "real: the view", "route": "#/models/deleted", "status": "fetch",
			"expect": []string{"a.gguf", "2 profiles", "Kept until"}, "has": []string{"div.mr.del[data-trash='" + id + "'][data-state=trashed]"}},
		map[string]any{"name": "real: the panel", "route": "#/models/deleted?item=" + id, "status": "fetch",
			"page": []string{"by alex", "not used any more", "a1", "a2", "2,048", "8,192", "100 MiB", "200 MiB"}, "pageHas": []string{"aside.side button[data-act=restore]"}},
		map[string]any{"name": "real: delete b.gguf from its panel", "route": "#/models?model=b.gguf", "status": "fetch",
			"click":        []any{"aside.side button[data-act=delete]", map[string]any{"type": "div.dlg input[data-confirm]", "value": short}, comment("tidy up"), "div.dlg button[data-go]"},
			"fetchedQuery": []string{`^POST /api/models/delete/b\.gguf\?dry_run=1 `, `^POST /api/models/delete/b\.gguf \{"comment":"tidy up"\}$`},
			"expect":       []string{"Deleted b.gguf", "1 profile left the configuration"}},
		map[string]any{"name": "real: restore a.gguf", "route": "#/models/deleted?item=" + id, "status": "fetch",
			"click": []any{"aside.side button[data-act=restore]", comment("needed again"), "div.dlg button[data-go]"},
			"page":  []string{"Restored a.gguf", "2 profiles are back"}},
	)
	if !te.has("a.gguf") || te.has("b.gguf") {
		t.Fatalf("after the UI's delete and restore: a.gguf in the folder %v (want true), b.gguf %v (want false)", te.has("a.gguf"), te.has("b.gguf"))
	}
	if ids := te.profileIDs(); len(ids) != 2 || ids[0] != "a1" || ids[1] != "a2" {
		t.Fatalf("profiles after the UI's delete and restore: %v", ids)
	}

	// remove b.gguf for good from its panel: the dry run's sentence, the
	// name typed, a comment; the file is erased from the trash folder and
	// the list shows it under Removed for good
	recs, err := te.cs.TrashList(context.Background())
	if err != nil || len(recs) != 1 || recs[0].File != "b.gguf" {
		t.Fatalf("the trash before the remove: %v %v", recs, err)
	}
	bid := recs[0].ID
	// the remove answers slowly, as on the Linux gate (a race build and the
	// fsync of the trash folder, modelfiles/remove_linux.go:82): the page
	// must wait for the answer before it is judged (the harness waits for
	// requests to a real server still in flight)
	te.api.opts.trashStop = func(step string) bool {
		if step == "remove:after-record" {
			time.Sleep(400 * time.Millisecond)
		}
		return false
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": te.front.URL, "passWrites": []string{`^POST /api/models/trash/` + bid + `/remove`}, "cases": []any{
		map[string]any{"name": "real: remove b.gguf for good", "route": "#/models/deleted?item=" + bid, "status": "fetch",
			"click":        []any{"aside.side button[data-act=remove]", map[string]any{"type": "div.dlg input[data-confirm]", "value": "b.gguf"}, comment("free the disk"), "div.dlg button[data-go]"},
			"fetchedQuery": []string{`^POST /api/models/trash/` + bid + `/remove\?dry_run=1 `, `^POST /api/models/trash/` + bid + `/remove \{"comment":"free the disk"\}$`},
			"expect":       []string{"Removed b.gguf for good: under 1 MB freed", "Removed for good", "removed for good: free the disk", "under 1 MB"}, "absent": []string{"0.0 GB"}},
	}})
	if _, err := os.Lstat(filepath.Join(te.dir, ".trash", bid, "b.gguf")); !os.IsNotExist(err) {
		t.Fatalf("b.gguf is still in its trash folder after the UI's remove: %v", err)
	}
	if recs, _ := te.cs.TrashList(context.Background()); len(recs) != 0 {
		t.Fatalf("the trash after the remove: %v", recs)
	}

	// read-only: the server says models_writable false, and the dialog says
	// so at once, sending nothing
	te.ro = true
	rev := te.svc.Revision()
	run(map[string]any{"name": "real: the folder is read-only", "route": "#/models?model=a.gguf", "status": "fetch",
		"click":      []any{"aside.side button[data-act=delete]"},
		"page":       []string{"The models folder is read-only on this scheduler: deleting and restoring are not possible here. Nothing can be changed."},
		"pageHas":    []string{"div.dlg[data-dlg=delete][data-state=read-only] [data-ro-flag]"},
		"pageNone":   []string{"div.dlg input[data-confirm]", "div.dlg textarea"},
		"notFetched": []string{"^POST "}, "notOnPage": []string{"Deleted a.gguf"}})
	// the flag unknown (as on Windows): the dialog as before, and the
	// server's read_only refusal is its own state
	te.unknown = true
	run(map[string]any{"name": "real: read-only, the flag unknown", "route": "#/models?model=a.gguf", "status": "fetch",
		"click":   []any{"aside.side button[data-act=delete]", map[string]any{"type": "div.dlg input[data-confirm]", "value": shortOf(t, te, "a.gguf")}, comment("tidy"), "div.dlg button[data-go]"},
		"page":    []string{"The models folder is read-only on this scheduler: nothing was changed", "the models folder is read-only here: nothing was changed (mkdir"},
		"pageHas": []string{"div.dlg[data-dlg=delete][data-state=read-only]"}, "pageNone": []string{"[data-ro-flag]"}, "notOnPage": []string{"Deleted a.gguf"}})
	if !te.has("a.gguf") || te.svc.Revision() != rev {
		t.Fatal("a read-only delete from the UI changed something")
	}
}

// shortOf is the catalog's short name of a file (what the delete dialog asks to type)
func shortOf(t *testing.T, te *trashEnv, file string) string {
	t.Helper()
	if x := catalogKeys(t, te.env)[file]; x != nil {
		if s, _ := x["short_name"].(string); s != "" {
			return s
		}
	}
	return file
}

// Sizes in the trash's sentences: GB from 0.1 GB, MB below, "under 1 MB"
// below that; never "0.0 GB" (the reviewer, 26 Nov).
func TestTrashSizeText(t *testing.T) {
	for _, c := range []struct {
		b    int64
		want string
	}{{8_400_000_000, "8.4 GB"}, {100_000_000, "0.1 GB"}, {99_499_999, "99 MB"}, {52_400_000, "52 MB"}, {1_000_000, "1 MB"}, {999_999, "under 1 MB"}, {0, "under 1 MB"}} {
		if got := sizeText(c.b); got != c.want {
			t.Errorf("sizeText(%d) = %q, want %q", c.b, got, c.want)
		}
	}
}
