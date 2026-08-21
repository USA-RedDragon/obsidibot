-- name: EnsureBankAccount :exec
insert into bank_accounts (alderon_id) values ($1)
on conflict (alderon_id) do nothing;

-- name: GetBankAccount :one
select * from bank_accounts where alderon_id = $1;

-- LastOperationAt drives the per-user cooldown. Derived from the ledger rather
-- than a separate table, so there is nothing to keep in step with it.
-- name: LastOperationAt :one
select max(created_at)::timestamptz from bank_ledger where alderon_id = $1;

-- BeginOperation claims the player's single operation slot. It fails on
-- bank_ledger_one_inflight if one is already open, which is how concurrent
-- commands -- including ones landing on different replicas -- are serialised.
-- name: BeginOperation :one
insert into bank_ledger (
    alderon_id, discord_user_id, direction, amount, state, marks_before, interaction_token
) values ($1, $2, $3, $4, 'pending', $5, $6)
returning *;

-- MarkOperationInFlight is written BEFORE the RCON command is issued. From
-- here on the outcome is unknown until observed, and the command must never be
-- sent a second time.
-- name: MarkOperationInFlight :exec
update bank_ledger
   set state   = 'in_flight',
       sent_at = now()
 where id = $1 and state = 'pending';

-- name: GetOperation :one
select * from bank_ledger where id = $1;

-- CompleteOperation closes a confirmed transfer. Paired with the balance update
-- in one transaction by the caller, because a moved balance and an open row are
-- indistinguishable from theft.
-- name: CompleteOperation :exec
update bank_ledger
   set state       = 'applied',
       moved       = $2,
       marks_after = $3,
       resolved_at = now()
 where id = $1;

-- name: CreditBank :exec
update bank_accounts
   set balance    = balance + $2,
       updated_at = now()
 where alderon_id = $1;

-- DebitBank is guarded by the balance check as well as the row filter, so a
-- concurrent path can never drive it negative even if the caller's earlier
-- read said there was enough.
-- name: DebitBank :execrows
update bank_accounts
   set balance    = balance - $2,
       updated_at = now()
 where alderon_id = $1 and balance >= $2;

-- FailOperation closes a row whose command provably did not run. Nothing moved,
-- so the player is exactly where they started.
-- name: FailOperation :exec
update bank_ledger
   set state       = 'failed',
       error       = $2,
       resolved_at = now()
 where id = $1;

-- ParkForReview closes a row whose outcome could not be established. This is
-- the only state a human has to act on, and the only way marks can be wrong.
-- name: ParkForReview :exec
update bank_ledger
   set state       = 'needs_review',
       error       = $2,
       marks_after = $3,
       resolved_at = now()
 where id = $1;

-- name: RecordVerifyAttempt :one
update bank_ledger
   set verify_attempts = verify_attempts + 1
 where id = $1
returning verify_attempts;

-- UnresolvedOperations feeds the reconciler. SKIP LOCKED because rows are
-- independent per player: every replica can take a share, unlike the rating
-- applier which must have exactly one writer.
-- name: UnresolvedOperations :many
select * from bank_ledger
 where state in ('pending', 'in_flight')
   and created_at < now() - make_interval(secs => sqlc.arg(older_than_seconds)::int)
 order by created_at
 limit $1
   for update skip locked;

-- name: CountNeedsReview :one
select count(*) from bank_ledger where state = 'needs_review';
