package deploy

import "testing"

// Every branch of the release rules.
func TestDecideGate(t *testing.T) {
	held := gateInput{Gated: true, Watch: true, HasRan: true, Known: true, RanDigest: "old", Running: true, Digest: "new"}
	with := func(f func(*gateInput)) gateInput { in := held; f(&in); return in }
	for _, c := range []struct {
		name string
		in   gateInput
		want gateVerdict
	}{
		{"auto", with(func(in *gateInput) { in.Gated = false }), gateVerdict{}},
		{"watch: false", with(func(in *gateInput) { in.Watch = false }), gateVerdict{}},
		{"runs what the tag says", with(func(in *gateInput) { in.RanDigest = "new" }), gateVerdict{}},
		{"approved", with(func(in *gateInput) { in.Status = "approved" }), gateVerdict{}},
		{"approved, deploy failed", with(func(in *gateInput) { in.Status, in.Mode = "failed", "canary" }), gateVerdict{}},
		{"canary job", with(func(in *gateInput) { in.Status, in.Mode, in.Job = "pending", "canary", true }), gateVerdict{}},
		{"manual job", with(func(in *gateInput) { in.Status, in.Mode, in.Job = "pending", "manual", true }), gateVerdict{Gate: "held", Hold: true}},
		{"no release yet (racing the push)", held, gateVerdict{Gate: "held", Hold: true}},
		{"pending manual", with(func(in *gateInput) { in.Status, in.Mode = "pending", "manual" }), gateVerdict{Gate: "held", Hold: true}},
		{"rejected", with(func(in *gateInput) { in.Status, in.Mode = "rejected", "canary" }), gateVerdict{Gate: "held", Hold: true}},
		{"superseded", with(func(in *gateInput) { in.Status = "superseded" }), gateVerdict{Gate: "held", Hold: true}},
		{"pending canary, routed", with(func(in *gateInput) { in.Status, in.Mode, in.Canaryable = "pending", "canary", true }), gateVerdict{Gate: "held", Hold: true, Canary: true}},
		{"pending canary, worker", with(func(in *gateInput) { in.Status, in.Mode = "pending", "canary" }), gateVerdict{Gate: "held", Hold: true}},
		{"compose changed the image", with(func(in *gateInput) { in.Known, in.RanDigest = false, "" }), gateVerdict{Gate: "new"}},
		{"compose just added it", with(func(in *gateInput) { in.Known, in.HasRan, in.Running, in.RanDigest = false, false, false, "" }), gateVerdict{Gate: "new"}},
		{"just added, a push waits", with(func(in *gateInput) {
			in.Known, in.HasRan, in.Running, in.RanDigest, in.Status, in.Mode = false, false, false, "", "pending", "canary"
		}), gateVerdict{Gate: "new", Hold: true}},
		{"image changed, rejected digest", with(func(in *gateInput) { in.Known, in.RanDigest, in.Status = false, "", "rejected" }), gateVerdict{Gate: "new", Hold: true}},
		{"running, rows pruned", with(func(in *gateInput) { in.Known, in.HasRan, in.RanDigest = false, false, "" }), gateVerdict{Gate: "new", Hold: true}},
	} {
		if got := decideGate(c.in); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}
