package instance

import "testing"

// The load log also reads what the model server says about the host
// process's own memory: the recurrent state at load and every context
// checkpoint created or erased later (formats at llama.cpp a01665d30de2,
// tools/server/server-context.cpp 2689, 2727-2730, 4247; real lines carry a
// timestamp and level prefix).
func TestLoadLogReadsHostMemoryLines(t *testing.T) {
	l := NewLoadLog()
	for _, s := range []string{
		"0.01.119.248 I llama_memory_recurrent:        CPU RS buffer size =    50.25 MiB",
		"0.01.119.249 I llama_memory_recurrent: size =   50.25 MiB (     1 cells,  32 layers,  1 seqs), R (f32):    2.25 MiB, S (f32):   48.00 MiB",
		"1.00.000.000 I slot update_slots: id  0 | task 0 | created context checkpoint 1 of 32 (pos_min = 699, pos_max = 699, n_tokens = 700, size = 50.251 MiB)",
		"1.00.000.001 I slot update_slots: id  0 | task 1 | created context checkpoint 2 of 32 (pos_min = 1399, pos_max = 1399, n_tokens = 1400, size = 50.251 MiB)",
		"1.00.000.002 I slot update_slots: id  0 | task 2 | created context checkpoint 3 of 32 (pos_min = 2099, pos_max = 2099, n_tokens = 2100, size = 50.251 MiB)",
		"1.00.000.003 W slot update_slots: id  0 | task 3 | erasing old context checkpoint (pos_min = 699, pos_max = 699, n_tokens = 700, size = 50.251 MiB)",
		"1.00.000.004 I slot update_slots: id  0 | task 4 | erased invalidated context checkpoint (pos_min = 2099, pos_max = 2099, n_tokens = 2100, n_swa = 0, size = 50.251 MiB)",
		"1.00.000.005 I slot update_slots: id  0 | task 4 | restored context checkpoint (pos_min = 1399, pos_max = 1399, n_tokens = 1400, n_past = 1400, size = 50.251 MiB)",
	} {
		l.Feed(s)
	}
	h := l.Snapshot().HostMem
	if h.RecurrentMiB != 50.25 {
		t.Errorf("recurrent %v, want 50.25 (the size line, not the buffer line)", h.RecurrentMiB)
	}
	if h.CheckpointMiB != 50.251 || h.CheckpointsCreated != 3 || h.CheckpointsHeld != 1 {
		t.Errorf("%+v, want one checkpoint of 50.251 MiB held after 3 created and 2 erased", h)
	}
	if got := h.HeldMiB(); got < 50.2 || got > 50.3 {
		t.Errorf("held %v MiB", got)
	}
}
