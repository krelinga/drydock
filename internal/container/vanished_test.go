package container

import (
	"context"
	"strings"
	"testing"
)

// TestAVanishedContainerFailsNoOneElse: Docker answers a call naming one
// removed container with exit 1 and nothing for the others (measured on
// 29.8.2). Both readers ask once more without it, and the containers still
// there are read. A failure that names no vanished id is still a failure.
func TestAVanishedContainerFailsNoOneElse(t *testing.T) {
	ctx := context.Background()
	body := `for a; do [ "$a" = "` + idB + `" ] && { echo "Error response from daemon: No such container: ` + idB + `" >&2; exit 1; }; done
case "$1" in
stats) echo '{"ID":"` + idA + `","MemUsage":"2MiB / 1GiB"}' ;;
inspect) echo '[{"Id":"` + idA + `","SizeRw":10}]' ;;
esac`
	run, dir := fakes(t, map[string]string{"docker": body})
	m := Manager{Run: run}
	mem, err := m.Memory(ctx, []string{idA, idB})
	if err != nil || mem[idA] != 2<<20 || len(mem) != 1 {
		t.Errorf("memory: %v %v", mem, err)
	}
	sizes, err := m.WritableSizes(ctx, []string{idA, idB})
	if err != nil || sizes[idA] != 10 || len(sizes) != 1 {
		t.Errorf("sizes: %v %v", sizes, err)
	}
	if n := strings.Count(strings.Join(argv(t, dir, "docker"), " "), idB); n != 2 {
		t.Errorf("the vanished id was asked about %d times; want once per reader", n)
	}
	// Only the vanished one asked about: nothing to read, no error.
	if got, err := m.Memory(ctx, []string{idB}); err != nil || len(got) != 0 {
		t.Errorf("only a vanished id: %v %v", got, err)
	}
	// Control: a failure for another reason is not retried away.
	run, _ = fakes(t, map[string]string{"docker": "echo 'Cannot connect to the Docker daemon' >&2; exit 1"})
	if _, err := (Manager{Run: run}).Memory(ctx, []string{idA, idB}); err == nil {
		t.Error("a daemon failure was read as a vanished container")
	}
}
