package commands_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/commands"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/bwmarrin/discordgo"
)

// historyField is the embed field the recent-events block lives on.
const historyField = "Recent"

// statsInvoke builds a /stats interaction for the caller's own stats.
func statsInvoke() interactions.Context {
	return interactions.Context{
		UserID:  discordUser,
		GuildID: "g1",
		Interaction: &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionApplicationCommand,
			Data: discordgo.ApplicationCommandInteractionData{Name: "stats"},
		}},
	}
}

// pressInvoke builds a component interaction for a history button.
func pressInvoke(customID string) interactions.Context {
	return interactions.Context{
		UserID:  discordUser,
		GuildID: "g1",
		Interaction: &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionMessageComponent,
			Data: discordgo.MessageComponentInteractionData{CustomID: customID},
		}},
	}
}

// seedHistory gives the player a linked account and n kills against a rotating
// cast, each with a recorded rating move so the history has figures to show.
func seedHistory(t *testing.T, h *linkHarness, n int) {
	t.Helper()
	ctx := context.Background()
	q := h.store.Queries()

	for _, agid := range []string{testAGID, "555-000-202", "555-000-303"} {
		if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
			AlderonID: agid, LastKnownName: "player-" + agid, Rating: 1200,
		}); err != nil {
			t.Fatalf("seed player: %v", err)
		}
	}
	if err := q.CreateLink(ctx, gen.CreateLinkParams{
		DiscordUserID: discordUser, AlderonID: testAGID,
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	for i := range n {
		victim := "555-000-202"
		if i%2 == 1 {
			victim = "555-000-303"
		}
		before, after := 1200.0+float64(i), 1210.0+float64(i)
		victimAfter := before - 10
		insertRatedKill(t, h, fmt.Sprintf("stats-%04d-padding-to-32-bytes!", i),
			testAGID, victim, &before, &after, &before, &victimAfter)
	}
}

// insertRatedKill records an event that has already been rated and posted,
// which is what every row /stats reads looks like.
//
// The flags are parameters rather than literals so the three call sites share
// one statement -- and so the SQL does not carry a run of identical boolean
// words, which is unreadable and which the linter is right to dislike.
func insertRatedKill(t *testing.T, h *linkHarness, dedupe, killerAGID, victimAGID string,
	killerBefore, killerAfter, victimBefore, victimAfter *float64,
) {
	t.Helper()
	const stmt = `
		insert into kill_events (dedupe_key, server_guid, payload, victim_agid, victim_name,
		                         damage_type, killer_agid, killer_name, credited, counts_death,
		                         killer_rating_before, killer_rating_after,
		                         victim_rating_before, victim_rating_after, rated, posted)
		values ($1, 'guid', '{}'::jsonb, $2, 'victim', 'DT_ATTACK', $3, 'killer', $4, $4,
		        $5, $6, $7, $8, $4, $4)`
	if _, err := h.pool.Exec(context.Background(), stmt,
		[]byte(dedupe), victimAGID, killerAGID, true,
		killerBefore, killerAfter, victimBefore, victimAfter); err != nil {
		t.Fatalf("seed event: %v", err)
	}
}

// TestStatsIsEphemeral: the history names whoever killed you, and a public
// message naming people is one that eventually pings somebody.
func TestStatsIsEphemeral(t *testing.T) {
	h := newLinkHarness(t)
	if !commands.NewStats(h.store).Command().Ephemeral {
		t.Error("/stats is public; its history names other players")
	}
}

// TestStatsShowsRecentHistory is the defensibility requirement: the rating is a
// claim, and these lines are the evidence for it.
func TestStatsShowsRecentHistory(t *testing.T) {
	h := newLinkHarness(t)
	seedHistory(t, h, 3)

	reply, err := commands.NewStats(h.store).Command().Handler(context.Background(), statsInvoke())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(reply.Embeds) != 1 {
		t.Fatalf("%d embeds, want 1", len(reply.Embeds))
	}

	var history string
	for _, field := range reply.Embeds[0].Fields {
		if field.Name == historyField {
			history = field.Value
		}
	}
	if history == "" {
		t.Fatal("no recent history on the embed")
	}
	if lines := strings.Count(history, "\n") + 1; lines != 3 {
		t.Errorf("%d history lines, want 3", lines)
	}
	if !strings.Contains(history, "killed") {
		t.Errorf("history does not say what happened: %q", history)
	}
	if !strings.Contains(history, "→") {
		t.Errorf("history does not show the rating move: %q", history)
	}
}

// TestStatsShowsBothKillsAndDeaths: a player asking why they dropped forty
// points is asking about the deaths, so a kills-only list cannot answer them.
func TestStatsShowsBothKillsAndDeaths(t *testing.T) {
	h := newLinkHarness(t)
	seedHistory(t, h, 1)

	ctx := context.Background()
	// One where they were on the receiving end.
	killerBefore, killerAfter := 1200.0, 1212.0
	victimBefore, victimAfter := 1300.0, 1288.0
	insertRatedKill(t, h, "death-row-padding-to-32-bytes!!!", "555-000-202", testAGID,
		&killerBefore, &killerAfter, &victimBefore, &victimAfter)

	reply, err := commands.NewStats(h.store).Command().Handler(ctx, statsInvoke())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	var history string
	for _, field := range reply.Embeds[0].Fields {
		if field.Name == historyField {
			history = field.Value
		}
	}
	if !strings.Contains(history, "killed by") {
		t.Errorf("the death is not shown: %q", history)
	}
	if !strings.Contains(history, "−") {
		t.Errorf("the rating loss is not shown as a loss: %q", history)
	}
}

// TestStatsOffersViewMoreOnlyWhenThereIsMore: a button leading to the same five
// lines is a dead end.
func TestStatsOffersViewMoreOnlyWhenThereIsMore(t *testing.T) {
	h := newLinkHarness(t)
	seedHistory(t, h, 3)
	stats := commands.NewStats(h.store)

	reply, err := stats.Command().Handler(context.Background(), statsInvoke())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(reply.Components) != 0 {
		t.Error("a View more button was offered for three events")
	}

	h2 := newLinkHarness(t)
	seedHistory(t, h2, 12)
	reply, err = commands.NewStats(h2.store).Command().Handler(context.Background(), statsInvoke())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(reply.Components) == 0 {
		t.Error("no View more button for twelve events")
	}
}

// TestStatsWithNoHistoryRendersNoBlock: a heading over nothing reads as a fault.
func TestStatsWithNoHistoryRendersNoBlock(t *testing.T) {
	h := newLinkHarness(t)
	seedHistory(t, h, 0)

	reply, err := commands.NewStats(h.store).Command().Handler(context.Background(), statsInvoke())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	for _, field := range reply.Embeds[0].Fields {
		if field.Name == historyField {
			t.Errorf("an empty history block was rendered: %q", field.Value)
		}
	}
	if len(reply.Components) != 0 {
		t.Error("a View more button was offered with no history")
	}
}

// TestHistoryPaginationIsStable is what the anchor buys.
//
// Without it, a kill landing while somebody pages shifts every row down and
// they see the same event on two pages -- which looks exactly like the bug a
// player checking up on their rating is hoping to find.
func TestHistoryPaginationIsStable(t *testing.T) {
	h := newLinkHarness(t)
	seedHistory(t, h, 25)
	stats := commands.NewStats(h.store)

	reply, err := stats.Command().Handler(context.Background(), statsInvoke())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	first := pageThrough(t, stats, buttonID(t, reply))

	// A fresh kill arrives mid-browse.
	insertRatedKill(t, h, "interloper-padding-to-32-bytes!!", testAGID, "555-000-202",
		nil, nil, nil, nil)

	second := pageThrough(t, stats, buttonID(t, reply))
	if first != second {
		t.Errorf("paging changed under a concurrent kill:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// buttonID digs the custom ID out of the first button on a reply.
func buttonID(t *testing.T, reply interactions.Reply) string {
	t.Helper()
	if len(reply.Components) == 0 {
		t.Fatal("no components on the reply")
	}
	row, ok := reply.Components[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("component is %T, want an ActionsRow", reply.Components[0])
	}
	button, ok := row.Components[0].(discordgo.Button)
	if !ok {
		t.Fatalf("component is %T, want a Button", row.Components[0])
	}
	return button.CustomID
}

// pageThrough walks Older until it runs out, returning everything it saw.
func pageThrough(t *testing.T, stats *commands.Stats, customID string) string {
	t.Helper()
	var seen []string
	for range 10 {
		reply, err := stats.Component().Handler(context.Background(), pressInvoke(customID))
		if err != nil {
			t.Fatalf("history page: %v", err)
		}
		if len(reply.Embeds) == 0 {
			t.Fatalf("no embed on a history page: %+v", reply)
		}
		seen = append(seen, reply.Embeds[0].Description)

		row, ok := reply.Components[0].(discordgo.ActionsRow)
		if !ok {
			t.Fatalf("component is %T", reply.Components[0])
		}
		older, ok := row.Components[1].(discordgo.Button)
		if !ok {
			t.Fatalf("component is %T", row.Components[1])
		}
		if older.Disabled {
			break
		}
		customID = older.CustomID
	}
	return strings.Join(seen, "\n--- page ---\n")
}

// TestHistoryButtonsAreRefusedWhenMalformed: the custom ID is a string a client
// sent us, so it is input.
func TestHistoryButtonsAreRefusedWhenMalformed(t *testing.T) {
	h := newLinkHarness(t)
	seedHistory(t, h, 3)
	stats := commands.NewStats(h.store)

	for _, customID := range []string{
		"hist", "hist:", "hist:555-000-101", "hist:555-000-101:notanumber:0",
		"hist:555-000-101:5:notanumber", "hist:not-an-agid:5:0", "hist:555-000-101:5:0:extra",
		"hist:555-000-101:-1:0", "hist:555-000-101:5:-1",
	} {
		reply, err := stats.Component().Handler(context.Background(), pressInvoke(customID))
		if err != nil {
			t.Fatalf("%q returned an error rather than a refusal: %v", customID, err)
		}
		if !reply.UserError {
			t.Errorf("%q was accepted", customID)
		}
	}
}
