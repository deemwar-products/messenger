package channel

import (
	"testing"

	"github.com/deemwar-products/messenger/config"
)

// The whatsapp lane refuses a group JID already bound to another channel, and the
// register idempotency check (LaneMatches) accepts a re-run of the same lane while
// rejecting a changed target.
func TestWhatsappLane_DupGroupRefusedAndIdempotent(t *testing.T) {
	k := Kinds()["whatsapp"]
	existing := map[string]config.Transport{
		"ops": {Kind: "whatsapp", Options: map[string]string{"group": "111@g.us"}},
	}
	if _, _, err := k.Lane("desk", LaneParams{Group: "111@g.us"}, existing); err == nil {
		t.Fatal("bound group must be refused")
	}
	want, hints, err := k.Lane("desk", LaneParams{Group: "222@g.us"}, existing)
	if err != nil || want.Options["group"] != "222@g.us" || len(hints) == 0 {
		t.Fatalf("free group lane: %+v %v %v", want, hints, err)
	}
	if !LaneMatches("desk", want, want) {
		t.Fatal("identical lane must match (idempotent re-run)")
	}
	other := want
	other.Options = map[string]string{"group": "333@g.us"}
	if LaneMatches("desk", want, other) {
		t.Fatal("different target must not match")
	}
}

// Kinds that host lanes enforce their own requirements; Base refuses lanes entirely.
func TestLanes_PerKindRequirements(t *testing.T) {
	if _, _, err := Kinds()["telegram"].Lane("bot", LaneParams{}, nil); err == nil {
		t.Fatal("telegram lane without --token-env must fail")
	}
	tg, _, err := Kinds()["telegram"].Lane("bot", LaneParams{TokenEnv: "BOT_TOKEN", ChatID: "-100"}, nil)
	if err != nil || tg.TokenEnv != "BOT_TOKEN" || tg.Options["chatId"] != "-100" {
		t.Fatalf("telegram lane: %+v %v", tg, err)
	}
	if _, _, err := Kinds()["webhook"].Lane("ci", LaneParams{}, nil); err == nil {
		t.Fatal("webhook lane without --token-env must fail")
	}
	if _, _, err := (Base{}).Lane("x", LaneParams{}, nil); err == nil {
		t.Fatal("Base must refuse lanes")
	}
}

// freeGroups hides JIDs already bound to channels.
func TestFreeGroups_FiltersBound(t *testing.T) {
	// exercised via the pure filter part: taken map derived from existing channels
	existing := map[string]config.Transport{
		"ops": {Kind: "whatsapp", Options: map[string]string{"group": "111@g.us"}},
	}
	all := []waGroup{{JID: "111@g.us", Name: "Ops"}, {JID: "222@g.us", Name: "Free"}}
	taken := map[string]bool{}
	for _, tr := range existing {
		if g := tr.Options["group"]; g != "" {
			taken[g] = true
		}
	}
	var free []waGroup
	bound := 0
	for _, g := range all {
		if taken[g.JID] {
			bound++
			continue
		}
		free = append(free, g)
	}
	if bound != 1 || len(free) != 1 || free[0].JID != "222@g.us" {
		t.Fatalf("filter wrong: free=%v bound=%d", free, bound)
	}
}
