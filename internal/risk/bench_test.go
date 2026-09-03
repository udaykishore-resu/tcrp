package risk

import (
	"context"
	"fmt"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/money"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// widen grows the seeded portfolio by cloning its counterparties, so the
// benchmarks measure something closer to a real book than the eight-name
// fixture without inventing a second dataset to keep in step with the first.
func widenSnapshot(base domain.Snapshot, copies int) domain.Snapshot {
	out := base
	out.Counterparties = nil
	out.Positions = nil
	out.Obligations = nil
	for c := 0; c < copies; c++ {
		suffix := fmt.Sprintf("-%02d", c)
		for _, cp := range base.Counterparties {
			cp.ID += suffix
			out.Counterparties = append(out.Counterparties, cp)
		}
		for i, p := range base.Positions {
			p.ID = fmt.Sprintf("%s%s-%d", p.ID, suffix, i)
			p.Counterparty += suffix
			out.Positions = append(out.Positions, p)
		}
		for i, o := range base.Obligations {
			o.ID = fmt.Sprintf("%s%s-%d", o.ID, suffix, i)
			o.Counterparty += suffix
			o.NettingSet += suffix
			out.Obligations = append(out.Obligations, o)
		}
	}
	return out
}

func benchEngine() *Engine {
	return New(obs.NewManualClock(domain.SeedTime), nil, obs.NewRegistry(), nil)
}

func BenchmarkExposureAll(b *testing.B) {
	base := domain.SnapshotFromSeed(domain.NewSeed())
	for _, copies := range []int{1, 8, 32} {
		snap := widenSnapshot(base, copies)
		b.Run(fmt.Sprintf("counterparties=%d", len(snap.Counterparties)), func(b *testing.B) {
			e := benchEngine()
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := e.ExposureAll(ctx, snap); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkExposureSingle(b *testing.B) {
	snap := domain.SnapshotFromSeed(domain.NewSeed())
	e := benchEngine()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Exposure(ctx, snap, "CP-BRAVO"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStress(b *testing.B) {
	base := domain.SnapshotFromSeed(domain.NewSeed())
	sc, ok := domain.NewSeed().Scenario("redemption_run")
	if !ok {
		b.Fatal("no redemption_run scenario")
	}
	for _, copies := range []int{1, 8, 32} {
		snap := widenSnapshot(base, copies)
		b.Run(fmt.Sprintf("counterparties=%d", len(snap.Counterparties)), func(b *testing.B) {
			e := benchEngine()
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := e.Stress(ctx, snap, sc); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkStressAll(b *testing.B) {
	snap := domain.SnapshotFromSeed(domain.NewSeed())
	scenarios := domain.NewSeed().Scenarios
	e := benchEngine()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.StressAll(ctx, snap, scenarios); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCheckLimits(b *testing.B) {
	snap := domain.SnapshotFromSeed(domain.NewSeed())
	e := benchEngine()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.CheckLimits(ctx, snap); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFormatExposure(b *testing.B) {
	snap := domain.SnapshotFromSeed(domain.NewSeed())
	e := benchEngine()
	x, err := e.Exposure(context.Background(), snap, "CP-DELTA")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FormatExposure(x)
	}
}

// BenchmarkShare measures the exact-rational share arithmetic on its own, since
// it runs once per collateral line per concentration pass and is the one place
// where insisting on exactness could plausibly cost something.
func BenchmarkShare(b *testing.B) {
	v := money.FromUnits(money.USD, 246_337_500_000_000)
	total := money.FromUnits(money.USD, 360_150_887_700_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = share(v, total)
	}
}
