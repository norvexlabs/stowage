package install

import (
	"strings"
	"testing"
)

// TestEnvFileCheckRefusesTotal: the recorded values' sum is bounded at maxEnvTotal, on both sides.
func TestEnvFileCheckRefusesTotal(t *testing.T) {
	t.Parallel()
	fill := func(total int) *EnvFile {
		e := NewEnvFile()
		for i := 0; total > 0; i++ {
			k := "K" + strings.Repeat("A", i%5) + string(rune('A'+i/5%26)) + string(rune('A'+i/130))
			n := min(total-len(k)-2, 60<<10)
			e.Set(k, strings.Repeat("v", n))
			total -= len(k) + n + 2
		}
		return e
	}
	if err := fill(maxEnvTotal).Check(); err != nil {
		t.Fatalf("a file of exactly %d bytes refused: %v", maxEnvTotal, err)
	}
	err := fill(maxEnvTotal + 1).Check()
	if err == nil || !strings.Contains(err.Error(), "shorter values") {
		t.Fatalf("a file one byte over: %v", err)
	}
}
