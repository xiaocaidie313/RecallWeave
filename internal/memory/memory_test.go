package memory

import (
	"math"
	"testing"
	"time"
)

func TestActivationMultiplier(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	t.Run("never recalled keeps neutral weight", func(t *testing.T) {
		got := activationMultiplier(0, time.Time{}, now)
		if got != 1 {
			t.Fatalf("expected 1, got %v", got)
		}
	})

	t.Run("frequent recall boosts above 1", func(t *testing.T) {
		before := now.Add(-1 * time.Hour) // 一小时内刚命中过
		got := activationMultiplier(10, before, now)
		if got <= 1 {
			t.Fatalf("expected boost > 1, got %v", got)
		}
	})

	t.Run("recent recall outranks stale recall", func(t *testing.T) {
		recent := activationMultiplier(10, now.Add(-1*time.Hour), now)
		stale := activationMultiplier(10, now.Add(-30*24*time.Hour), now) // 30 天没命中
		if stale >= recent {
			t.Fatalf("expected stale < recent, stale=%v recent=%v", stale, recent)
		}
	})

	t.Run("log1p tames the boost growth", func(t *testing.T) {
		base := activationMultiplier(0, now.Add(-1*time.Hour), now)
		low := activationMultiplier(1, now.Add(-1*time.Hour), now)
		huge := activationMultiplier(1000, now.Add(-1*time.Hour), now)
		// 从 1 次到 1000 次，乘数不能按线性暴涨（否则单条记忆永远霸榜）。
		if (huge-base)/(low-base) >= 50 {
			t.Fatalf("boost grew too fast: base=%v low=%v huge=%v", base, low, huge)
		}
		if math.IsNaN(huge) || math.IsInf(huge, 0) {
			t.Fatalf("boost produced a non-finite value: %v", huge)
		}
	})
}
