package godot

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPruneFrames(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 6; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("frame%08d.png", i)), nil, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "frame.wav"), nil, 0o644)
	wanted := map[int]bool{1: true, 5: true}

	// Во время записи самый новый кадр (5) может быть недописан — его не трогаем.
	kept := pruneFrames(dir, wanted, false)
	if len(kept) != 2 {
		t.Fatalf("kept = %v", kept)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "frame*.png"))
	if len(left) != 2 {
		t.Errorf("after pruning: %v", left)
	}
	os.WriteFile(filepath.Join(dir, "frame00000006.png"), nil, 0o644)
	if kept := pruneFrames(dir, wanted, true); len(kept) != 2 {
		t.Errorf("final kept = %v", kept)
	}
	if _, err := os.Stat(filepath.Join(dir, "frame00000006.png")); err == nil {
		t.Error("final pass must remove unwanted newest frame")
	}
}
