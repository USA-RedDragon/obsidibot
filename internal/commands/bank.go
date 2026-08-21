package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/USA-RedDragon/obsidibot/internal/bank"
	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
)

// Banker implements /deposit, /withdraw and /balance.
type Banker struct {
	store *db.Store
	bank  *bank.Bank
	cfg   *config.Config
}

// NewBanker builds the banking handlers.
func NewBanker(store *db.Store, b *bank.Bank, cfg *config.Config) *Banker {
	return &Banker{store: store, bank: b, cfg: cfg}
}

// amountOption is the shared shape of the amount argument.
//
// It is NOT required: omitting it means "all", which is the common case and
// saves a player working out their own balance. MinValue pushes the "must be
// positive" rule into Discord's own client-side validation so an obvious
// mistake never becomes a round trip.
func amountOption(description string) *discordgo.ApplicationCommandOption {
	minimum := 1.0
	return &discordgo.ApplicationCommandOption{
		Name:        "amount",
		Description: description,
		Type:        discordgo.ApplicationCommandOptionInteger,
		Required:    false,
		MinValue:    &minimum,
	}
}

// Commands returns the three banking commands.
//
// All are deferred (each makes two or three RCON round trips, well past
// Discord's three-second budget) and ephemeral (a player's balance is theirs).
func (b *Banker) Commands() []interactions.Command {
	return []interactions.Command{
		{
			Defer:     true,
			Ephemeral: true,
			Definition: &discordgo.ApplicationCommand{
				Name:        "deposit",
				Description: "Move marks from the dinosaur you are playing into your bank",
				Options:     []*discordgo.ApplicationCommandOption{amountOption("How many marks to deposit (default: all of them)")},
			},
			Handler: b.deposit,
		},
		{
			Defer:     true,
			Ephemeral: true,
			Definition: &discordgo.ApplicationCommand{
				Name:        "withdraw",
				Description: "Move marks from your bank onto the dinosaur you are playing",
				Options:     []*discordgo.ApplicationCommandOption{amountOption("How many marks to withdraw (default: all of them)")},
			},
			Handler: b.withdraw,
		},
		{
			Defer:     true,
			Ephemeral: true,
			Definition: &discordgo.ApplicationCommand{
				Name:        "balance",
				Description: "Show how many marks you have banked",
			},
			Handler: b.balance,
		},
	}
}

func (b *Banker) deposit(ctx context.Context, ic interactions.Context) (interactions.Reply, error) {
	return b.move(ctx, ic, gen.BankDirectionDeposit)
}

func (b *Banker) withdraw(ctx context.Context, ic interactions.Context) (interactions.Reply, error) {
	return b.move(ctx, ic, gen.BankDirectionWithdraw)
}

func (b *Banker) move(ctx context.Context, ic interactions.Context,
	direction gen.BankDirection,
) (interactions.Reply, error) {
	link, err := b.store.Queries().GetLinkByDiscordID(ctx, ic.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return notLinked(), nil
	}
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("look up link: %w", err)
	}

	amount := bank.AmountAll
	if raw := optionInt(ic.Interaction.ApplicationCommandData().Options, "amount"); raw != nil {
		amount = *raw
	}

	// The interaction token is stored on the ledger row so a reconciler can
	// find its way back to the message the player is looking at.
	token := ic.Interaction.Token

	var result bank.Result
	if direction == gen.BankDirectionDeposit {
		result, err = b.bank.Deposit(ctx, ic.UserID, link.AlderonID, token, amount)
	} else {
		result, err = b.bank.Withdraw(ctx, ic.UserID, link.AlderonID, token, amount)
	}
	if err != nil {
		return b.explain(err, direction), nil
	}

	verb, preposition := "Deposited", "into your bank"
	if direction == gen.BankDirectionWithdraw {
		verb, preposition = "Withdrew", "onto your dinosaur"
	}
	content := fmt.Sprintf("%s **%s** marks %s.\nBank: **%s** · In game: **%s**",
		verb, commas(result.Moved), preposition, commas(result.Balance), commas(result.InGame))
	if result.Clamped {
		content += "\n*(That was everything available, which was less than you asked for.)*"
	}
	return interactions.Reply{Content: content}, nil
}

func (b *Banker) balance(ctx context.Context, ic interactions.Context) (interactions.Reply, error) {
	q := b.store.Queries()
	link, err := q.GetLinkByDiscordID(ctx, ic.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return notLinked(), nil
	}
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("look up link: %w", err)
	}

	if err := q.EnsureBankAccount(ctx, link.AlderonID); err != nil {
		return interactions.Reply{}, fmt.Errorf("ensure bank account: %w", err)
	}
	account, err := q.GetBankAccount(ctx, link.AlderonID)
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("read balance: %w", err)
	}

	// Reporting the in-game figure too makes the whole picture visible in one
	// command, but it needs the player online, so its absence is not an error.
	content := fmt.Sprintf("You have **%s** marks banked.", commas(account.Balance))
	if player, err := b.bank.InGameMarks(ctx, link.AlderonID); err == nil {
		content += fmt.Sprintf("\nOn your current dinosaur: **%s**.", commas(player))
	} else if errors.Is(err, pot.ErrPlayerNotOnline) {
		content += "\n*Log in to see and move the marks on your dinosaur.*"
	}
	return interactions.Reply{Content: content}, nil
}

// explain turns a banking failure into something a player can act on.
//
// The needs_review case is deliberately explicit rather than a generic error:
// the player's marks may be in an odd state, and telling them "something went
// wrong" while a moderator quietly fixes it is how a support problem becomes a
// trust problem.
func (b *Banker) explain(err error, direction gen.BankDirection) interactions.Reply {
	switch {
	case errors.Is(err, bank.ErrBusy):
		return userError("You already have a transfer in progress. Give it a moment.")
	case errors.Is(err, bank.ErrTooSoon):
		return userError(fmt.Sprintf("Too soon — wait %d seconds between transfers.",
			b.cfg.Bank.CooldownSeconds))
	case errors.Is(err, bank.ErrNothingToMove):
		if direction == gen.BankDirectionDeposit {
			return userError("Your dinosaur has no marks to deposit.")
		}
		return userError("You have no marks banked to withdraw.")
	case errors.Is(err, bank.ErrNeedsReview):
		return interactions.Reply{Content: "The game server did not confirm that transfer, so the bot has " +
			"**not** changed your balance and has flagged it for a moderator to check. " +
			"Please do not retry it — check `/balance` and your in-game marks first."}
	default:
		return rconReply(err, "banking transfer")
	}
}

func notLinked() interactions.Reply {
	return userError("You need to link your account first. Run `/link start` while you are in game.")
}

// optionInt reads an integer option, returning nil when it was omitted.
func optionInt(options []*discordgo.ApplicationCommandInteractionDataOption, name string) *int64 {
	for _, opt := range options {
		if opt.Name == name {
			v := opt.IntValue()
			return &v
		}
	}
	return nil
}

// commas groups thousands, because marks balances run to five and six digits
// and "1247300" is not a number anybody reads correctly at a glance.
func commas(v int64) string {
	digits := fmt.Sprintf("%d", v)
	sign := ""
	if v < 0 {
		sign, digits = "-", digits[1:]
	}
	if len(digits) <= 3 {
		return sign + digits
	}
	var out []byte
	for i, d := range []byte(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, d)
	}
	return sign + string(out)
}
