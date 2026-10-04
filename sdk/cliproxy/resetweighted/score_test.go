package resetweighted

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)

func week(used float64, resetsIn time.Duration) Window {
	return Window{Used: used, Known: true, Span: spanWeek, ResetsAt: t0.Add(resetsIn)}
}

func fiveH(used float64) Window {
	return Window{Used: used, Known: true, Span: span5h, ResetsAt: t0.Add(2 * time.Hour)}
}

func q(long, short Window) *Quota {
	return &Quota{Long: long, Short: short, ObservedAt: t0}
}

func cand(id string, quota *Quota) Candidate {
	return Candidate{ID: id, Provider: "codex", Quota: quota}
}

func ids(ranked []Scored) []string {
	out := make([]string, 0, len(ranked))
	for _, s := range ranked {
		out = append(out, s.ID)
	}
	return out
}

func TestUrgencyRampsOverHorizon(t *testing.T) {
	h := 72 * time.Hour
	cases := []struct {
		in   time.Duration
		want float64
	}{
		{96 * time.Hour, 0}, {72 * time.Hour, 0}, {36 * time.Hour, 0.5}, {7 * time.Hour, 65.0 / 72.0}, {0, 0},
	}
	for _, c := range cases {
		got := Urgency(t0.Add(c.in), t0, h)
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("urgency(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	if Urgency(time.Time{}, t0, h) != 0 {
		t.Fatal("unknown reset must not be urgent")
	}
}

// Live-shaped table from the spec: f10 (87% unspent, resets in 7h) must win over
// f1 (4% unspent, 4.2d) and f9 (97% unspent but 1.9d away) and the far-reset idle seats.
func TestRankNearResetWithHeadroomWins(t *testing.T) {
	cfg := DefaultScoreConfig()
	cands := []Candidate{
		cand("f1", q(week(96, 4*24*time.Hour+5*time.Hour), fiveH(10))),
		cand("f9", q(week(3, 46*time.Hour), fiveH(10))),
		cand("f10", q(week(13, 7*time.Hour), fiveH(10))),
		cand("far", q(week(0, 6*24*time.Hour), fiveH(0))),
	}
	got := ids(Rank(cfg, cands, t0, false))
	if got[0] != "f10" {
		t.Fatalf("rank = %v, want f10 first", got)
	}
	if got[1] != "f9" {
		t.Fatalf("rank = %v, want f9 second (97%% headroom x 0.36 urgency)", got)
	}
}

func TestRankDrainedNearResetStaysBack(t *testing.T) {
	cfg := DefaultScoreConfig()
	cands := []Candidate{
		cand("drained", q(week(99, 2*time.Hour), fiveH(10))),
		cand("idle-far", q(week(10, 6*24*time.Hour), fiveH(10))),
	}
	got := ids(Rank(cfg, cands, t0, false))
	// high urgency x ~0 headroom = ~0 (inside score_eps of the far seat) -> tie ->
	// most limiting headroom wins -> idle-far
	if got[0] != "idle-far" {
		t.Fatalf("rank = %v, want idle-far first", got)
	}
}

func TestCappedNeverPicked(t *testing.T) {
	cfg := DefaultScoreConfig()
	capped := q(week(100, 3*time.Hour), fiveH(0))
	capped.Long.Rejected = true
	cands := []Candidate{cand("capped", capped), cand("ok", q(week(50, 5*24*time.Hour), fiveH(0)))}
	got := ids(Rank(cfg, cands, t0, false))
	if len(got) != 1 || got[0] != "ok" {
		t.Fatalf("rank = %v, want only ok", got)
	}
	// A 5h cap counts too.
	shortCapped := q(week(10, 3*time.Hour), fiveH(100))
	shortCapped.Short.Rejected = true
	got = ids(Rank(cfg, []Candidate{cand("sc", shortCapped), cand("ok", q(week(50, 5*24*time.Hour), fiveH(0)))}, t0, false))
	if len(got) != 1 || got[0] != "ok" {
		t.Fatalf("rank = %v, want only ok", got)
	}
}

func TestFiveHourGuardExcludesWhenAlternativeExists(t *testing.T) {
	cfg := DefaultScoreConfig()
	hot := cand("hot", q(week(10, 3*time.Hour), fiveH(90))) // best reclaim, but over 85% 5h
	cool := cand("cool", q(week(40, 5*24*time.Hour), fiveH(20)))
	got := ids(Rank(cfg, []Candidate{hot, cool}, t0, false))
	if got[0] != "cool" || len(got) != 1 {
		t.Fatalf("rank = %v, want cool only", got)
	}
	// INV-1: alone, the guard relaxes.
	got = ids(Rank(cfg, []Candidate{hot}, t0, false))
	if len(got) != 1 || got[0] != "hot" {
		t.Fatalf("rank = %v, want hot (guard relaxed)", got)
	}
}

func TestUnknownQuotaFailsOpenToPriority(t *testing.T) {
	cfg := DefaultScoreConfig()
	unknown := Candidate{ID: "unknown", Priority: 5}
	known := cand("known", q(week(30, 5*24*time.Hour), fiveH(10)))
	known.Priority = 1
	ranked := Rank(cfg, []Candidate{unknown, known}, t0, false)
	if ranked[0].ID != "known" {
		t.Fatalf("rank = %v, want known first (unknown has 0 limiting headroom)", ids(ranked))
	}
	for _, s := range ranked {
		if s.ID == "unknown" && (s.Reason != "unknown_quota" || s.Reclaim != 0) {
			t.Fatalf("unknown scored %+v, want reclaim 0 / unknown_quota", s)
		}
	}
	// Stale snapshot == unknown.
	stale := q(week(5, 2*time.Hour), fiveH(0))
	stale.ObservedAt = t0.Add(-time.Hour)
	if cfg.Fresh(stale, t0) {
		t.Fatal("snapshot older than SnapshotMaxAge must be stale")
	}
	if _, _, rc := cfg.Reclaim(stale, t0); rc != 0 {
		t.Fatalf("stale reclaim = %v, want 0", rc)
	}
}

// Kimi: 30-day window, same formula; the horizon scales to 3/7 of the span (~12.86 d).
func TestKimiMonthlyHorizonScales(t *testing.T) {
	cfg := DefaultScoreConfig()
	month := func(used float64, resetsIn time.Duration) Window {
		return Window{Used: used, Known: true, Span: spanMonth, ResetsAt: t0.Add(resetsIn)}
	}
	// 10 days out on a 30-day window: inside the ~12.86d horizon -> urgent.
	_, urg, _ := cfg.Reclaim(&Quota{Long: month(20, 10*24*time.Hour), ObservedAt: t0}, t0)
	if urg <= 0.2 || urg >= 0.25 {
		t.Fatalf("kimi urgency at 10d = %v, want ~0.222 (horizon 12.86d)", urg)
	}
	// 10 days out on a 7-day-window formula would be 0; make sure the week horizon does NOT apply.
	_, urgWeek, _ := cfg.Reclaim(&Quota{Long: week(20, 10*24*time.Hour), ObservedAt: t0}, t0)
	if urgWeek != 0 {
		t.Fatalf("weekly urgency at 10d = %v, want 0", urgWeek)
	}
	// 20 days out: not urgent.
	_, urg, _ = cfg.Reclaim(&Quota{Long: month(20, 20*24*time.Hour), ObservedAt: t0}, t0)
	if urg != 0 {
		t.Fatalf("kimi urgency at 20d = %v, want 0", urg)
	}
}

func TestBalanceDampMovesPickOffLoadedSeat(t *testing.T) {
	cfg := DefaultScoreConfig()
	top := cand("top", q(week(10, 12*time.Hour), fiveH(10)))
	top.Inflight = 3
	idle := cand("idle", q(week(40, 30*time.Hour), fiveH(10)))
	got := ids(Rank(cfg, []Candidate{top, idle}, t0, false))
	if got[0] != "idle" {
		t.Fatalf("rank = %v, want idle first under inflight damp", got)
	}
	top.Inflight = 0
	got = ids(Rank(cfg, []Candidate{top, idle}, t0, false))
	if got[0] != "top" {
		t.Fatalf("rank = %v, want top first when idle", got)
	}
}

func TestTieBreakLimitingHeadroomThenPriority(t *testing.T) {
	cfg := DefaultScoreConfig()
	a := cand("a", q(week(50, 6*24*time.Hour), fiveH(70)))
	b := cand("b", q(week(50, 6*24*time.Hour), fiveH(20)))
	got := ids(Rank(cfg, []Candidate{a, b}, t0, false))
	if got[0] != "b" {
		t.Fatalf("rank = %v, want b (more 5h headroom)", got)
	}
	a.Quota.Short = fiveH(20)
	a.Priority, b.Priority = 1, 9
	got = ids(Rank(cfg, []Candidate{a, b}, t0, false))
	if got[0] != "b" {
		t.Fatalf("rank = %v, want b (higher host priority)", got)
	}
}

// Fable --------------------------------------------------------------------

func fableQ(total, fable float64, resetsIn time.Duration) *Quota {
	out := q(week(total, resetsIn), fiveH(10))
	out.Fable = Window{Used: fable, Known: true, Span: spanWeek, ResetsAt: t0.Add(resetsIn)}
	return out
}

func TestFableReservedPredicate(t *testing.T) {
	cfg := DefaultScoreConfig() // share 0.5, margin 5
	// total headroom 0.20, fable headroom 0.5*(1-0.6)=0.20 -> 0.20 <= 0.20+0.05 -> reserved
	if !cfg.FableReserved(fableQ(80, 60, 3*24*time.Hour), t0) {
		t.Fatal("expected reserved: total headroom down to the fable allowance")
	}
	// total headroom 0.60 >> fable 0.20+0.05 -> not reserved
	if cfg.FableReserved(fableQ(40, 60, 3*24*time.Hour), t0) {
		t.Fatal("expected headroom_ok")
	}
	// fable drained -> never reserved
	if cfg.FableReserved(fableQ(80, 100, 3*24*time.Hour), t0) {
		t.Fatal("expected fable_drained")
	}
	// fable row outside its week -> not measured
	if cfg.FableReserved(fableQ(80, 60, -time.Hour), t0) {
		t.Fatal("expected not_measured for a past-reset fable row")
	}
	// mode off -> never
	off := cfg
	off.FableReserveMode = "off"
	if off.FableReserved(fableQ(80, 60, 3*24*time.Hour), t0) {
		t.Fatal("mode off must never reserve")
	}
}

func TestFableWithholdAndTier(t *testing.T) {
	cfg := DefaultScoreConfig()
	reserved := cand("reserved", fableQ(80, 60, 3*time.Hour)) // near reset: would win on reclaim
	open := cand("open", fableQ(40, 60, 5*24*time.Hour))
	// Non-Fable work: reserved seat withheld.
	got := ids(Rank(cfg, []Candidate{reserved, open}, t0, false))
	if len(got) != 1 || got[0] != "open" {
		t.Fatalf("non-fable rank = %v, want open only", got)
	}
	// Fable work: reserved seat first (tier), draining its Fable before its reset.
	got = ids(Rank(cfg, []Candidate{open, reserved}, t0, true))
	if got[0] != "reserved" {
		t.Fatalf("fable rank = %v, want reserved first", got)
	}
	// Only reserved seats left for non-Fable work: withhold relaxes (never wedge).
	got = ids(Rank(cfg, []Candidate{reserved}, t0, false))
	if len(got) != 1 {
		t.Fatalf("rank = %v, want reserved when it is all that is left", got)
	}
	// Shadow mode: computed, not acted on.
	shadow := cfg
	shadow.FableReserveMode = "shadow"
	got = ids(Rank(shadow, []Candidate{reserved, open}, t0, false))
	if len(got) != 2 || got[0] != "reserved" {
		t.Fatalf("shadow rank = %v, want both with reserved first (reclaim)", got)
	}
}

// Comparison point from the store audit: quota-router protects claude-fable-* at a
// flat 50% weekly. Our predicate is headroom-relative: a sub at 55% weekly with
// its Fable allowance still mostly intact is NOT reserved (the flat rule would be
// wrong to withhold it), while a sub at 80% weekly with 40% Fable headroom IS.
func TestFableReservationVersusFlatFiftyPercentRule(t *testing.T) {
	cfg := DefaultScoreConfig()
	flatRule := func(q *Quota) bool { return q.Long.Used >= 50 }
	a := fableQ(55, 40, 4*24*time.Hour) // total headroom .45, fable units .30 (+.05 margin) -> open
	b := fableQ(80, 60, 4*24*time.Hour) // total headroom .20, fable units .20 -> reserved
	if cfg.FableReserved(a, t0) || !flatRule(a) {
		t.Fatalf("a: ours=%v flat=%v, want ours open while flat withholds", cfg.FableReserved(a, t0), flatRule(a))
	}
	if !cfg.FableReserved(b, t0) || !flatRule(b) {
		t.Fatalf("b: ours=%v flat=%v, want both reserved", cfg.FableReserved(b, t0), flatRule(b))
	}
}

func TestIsFableModel(t *testing.T) {
	prefixes := []string{"claude-fable"}
	for model, want := range map[string]bool{"claude-fable-5-1": true, "claude-fable-5": true, "Claude-Fable-5": true, "claude-sonnet-4-6": false, "": false} {
		if got := IsFableModel(model, prefixes); got != want {
			t.Fatalf("IsFableModel(%q) = %v, want %v", model, got, want)
		}
	}
}
