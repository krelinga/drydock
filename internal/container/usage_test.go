package container

import (
	"context"
	"strings"
	"testing"
)

var (
	idA = strings.Repeat("a", 64)
	idB = strings.Repeat("b", 64)
)

// TestMemoryReadsStatsJSON: one `docker stats` call names every container
// after "--", by full id, and each JSON line's MemUsage becomes bytes. A
// stopped container's "0B / 0B" is no reading — absent, not zero — while the
// running one beside it is read: the positive control in the same call.
func TestMemoryReadsStatsJSON(t *testing.T) {
	run, dir := fakes(t, map[string]string{"docker": `cat <<'EOF'
{"BlockIO":"0B / 0B","CPUPerc":"0.00%","Container":"` + idA + `","ID":"` + idA + `","MemPerc":"7.80%","MemUsage":"1.21GiB / 15.5GiB","Name":"x","NetIO":"1kB / 0B","PIDs":"9"}
{"BlockIO":"0B / 0B","CPUPerc":"0.00%","Container":"` + idB + `","ID":"` + idB + `","MemPerc":"0.00%","MemUsage":"0B / 0B","Name":"y","NetIO":"0B / 0B","PIDs":"0"}
EOF`})
	got, err := Manager{Run: run, LabelPrefix: "drydock.test"}.Memory(context.Background(), []string{idA, idB})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := got[idA]; !ok || v != 1299227607 {
		t.Errorf("running container: %d, %v", v, ok)
	}
	if v, ok := got[idB]; ok {
		t.Errorf("a stopped container reported %d bytes; it has no reading", v)
	}
	want := []string{"stats", "--no-stream", "--no-trunc", "--format", "{{json .}}", "--", idA, idB}
	if a := argv(t, dir, "docker"); strings.TrimSpace(strings.Join(a, " ")) != strings.Join(want, " ") {
		t.Errorf("argv %q, want %q", a, want)
	}
}

// TestMemoryRefusesWhatItCannotRead: an id that is not a full one is refused
// before docker runs; an answer about a container nobody asked about, or a
// MemUsage in a shape Docker does not print, is an error, never a guess; and
// a failed docker is an error. Each beside the same call made well, above.
func TestMemoryRefusesWhatItCannotRead(t *testing.T) {
	ctx := context.Background()
	line := func(id, mem string) string {
		return `{"ID":"` + id + `","MemUsage":"` + mem + `"}`
	}
	for name, body := range map[string]string{
		"an unasked id":   "echo '" + line(idB, "1MiB / 2GiB") + "'",
		"decimal units":   "echo '" + line(idA, "1.2GB / 2GB") + "'",
		"no limit half":   "echo '" + line(idA, "1.2GiB") + "'",
		"not JSON":        "echo 'CONTAINER ID   NAME   MEM USAGE'",
		"a failed docker": "echo 'Error: No such container' >&2; exit 1",
	} {
		run, _ := fakes(t, map[string]string{"docker": body})
		if got, err := (Manager{Run: run}).Memory(ctx, []string{idA}); err == nil {
			t.Errorf("%s: read %v", name, got)
		}
	}
	run, dir := fakes(t, map[string]string{"docker": "echo '" + line(idA, "512MiB / 2GiB") + "'"})
	m := Manager{Run: run}
	if _, err := m.Memory(ctx, []string{"--all"}); err == nil {
		t.Error("an option was accepted as an id")
	}
	if _, err := m.Memory(ctx, []string{idA[:12]}); err == nil {
		t.Error("a short id was accepted")
	}
	if got, err := m.Memory(ctx, []string{idA}); err != nil || got[idA] != 512<<20 {
		t.Errorf("control: %v, %v", got, err)
	}
	if got, err := m.Memory(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("no ids: %v, %v", got, err)
	}
	if n := len(argv(t, dir, "docker")); n == 0 {
		t.Error("control never ran docker")
	}
}

func TestParseMemUsage(t *testing.T) {
	for in, want := range map[string]uint64{
		"0B / 0B":           0,
		"432KiB / 108.1GiB": 432 << 10,
		"1.5MiB / 1GiB":     3 << 19,
		"2GiB / 4GiB":       2 << 30,
		"1023B / 1KiB":      1023,
	} {
		if got, _, err := ParseMemUsage(in); err != nil || got != want {
			t.Errorf("ParseMemUsage(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "1.2 GiB / 2GiB", "1.2GB / 2GB", "-1B / 2B", "1e3KiB / 1GiB", "1GiB/2GiB", "NaN / 1B"} {
		if v, _, err := ParseMemUsage(in); err == nil {
			t.Errorf("ParseMemUsage(%q) = %d, want an error", in, v)
		}
	}
}

// TestWritableSizesReadsSizeRw: `docker inspect --size` after "--", SizeRw
// per container; one without SizeRw is an error rather than a zero.
func TestWritableSizesReadsSizeRw(t *testing.T) {
	ctx := context.Background()
	run, dir := fakes(t, map[string]string{"docker": `echo '[{"Id":"` + idA + `","SizeRw":4096,"SizeRootFs":9999},{"Id":"` + idB + `","SizeRw":0}]'`})
	got, err := Manager{Run: run}.WritableSizes(ctx, []string{idA, idB})
	if err != nil || got[idA] != 4096 || got[idB] != 0 || len(got) != 2 {
		t.Fatalf("sizes %v, %v", got, err)
	}
	want := []string{"inspect", "--type", "container", "--size", "--", idA, idB}
	if a := argv(t, dir, "docker"); strings.TrimSpace(strings.Join(a, " ")) != strings.Join(want, " ") {
		t.Errorf("argv %q", a)
	}
	run, _ = fakes(t, map[string]string{"docker": `echo '[{"Id":"` + idA + `"}]'`})
	if got, err := (Manager{Run: run}).WritableSizes(ctx, []string{idA}); err == nil {
		t.Errorf("no SizeRw read as %v", got)
	}
	if _, err := (Manager{Run: run}).WritableSizes(ctx, []string{"x"}); err == nil {
		t.Error("a non-id was accepted")
	}
}
