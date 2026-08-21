-- name: EnsureBankAccount :exec
insert into bank_accounts (alderon_id) values ($1)
on conflict (alderon_id) do nothing;

-- name: GetBankAccount :one
select * from bank_accounts where alderon_id = $1;

-- LastOperationAt drives the per-user cooldown. Derived from the ledger rather
-- than a separate table, so there is nothing to keep in step with it.
--
-- DELIBERATELY NOT max(created_at). An aggregate over zero rows still returns
-- ONE ROW CONTAINING NULL, which sqlc scans into a plain time.Time and fails --
-- and that failure is not pgx.ErrNoRows, so the caller's "this player has no
-- history" branch never fires and every player's FIRST banking command is
-- rejected forever, because being rejected is also what stops them ever getting
-- a ledger row. Ordering by the index returns no row at all for a fresh player,
-- which is what the caller is written to expect.
-- name: LastOperationAt :one
select created_at from bank_ledger
 where alderon_id = $1
 order by created_at desc
 limit 1;

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
--
-- :execrows because the state guard is the whole point: 0 rows means the row is
-- no longer pending -- the reconciler closed it as failed while this request was
-- stalled -- and the caller MUST abandon the transfer without issuing anything.
-- Sending the command against a row that has already been closed would take a
-- player's marks with nothing left open to record it.
-- name: MarkOperationInFlight :execrows
update bank_ledger
   set state   = 'in_flight',
       sent_at = now()
 where id = $1 and state = 'pending';

-- name: GetOperation :one
select * from bank_ledger where id = $1;

-- CompleteOperation closes a confirmed transfer. Paired with the balance update
-- in one transaction by the caller, because a moved balance and an open row are
-- indistinguishable from theft.
--
-- The state guard makes the row itself the lock on the balance move. The
-- request path and the reconciler can both be holding the same transfer -- the
-- reconciler picks a row up on the assumption the request died, and it may not
-- have -- and whoever loses the race updates 0 rows and must roll back rather
-- than credit the balance a second time. Without it a slow-but-successful
-- transfer is credited twice, which mints currency.
-- name: CompleteOperation :execrows
update bank_ledger
   set state       = 'applied',
       moved       = $2,
       marks_after = $3,
       resolved_at = now()
 where id = $1 and state = 'in_flight';

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

-- FailOperation closes a row whose command WAS sent and came back refused: the
-- server answered that it did nothing, so the player is exactly where they
-- started. Only the request path can know that, which is why the guard is
-- in_flight -- it is the state that request put the row in before sending.
-- 0 rows means the row was resolved by someone else first.
-- name: FailOperation :execrows
update bank_ledger
   set state       = 'failed',
       error       = $2,
       resolved_at = now()
 where id = $1 and state = 'in_flight';

-- FailAbandonedOperation closes a row that never left pending, which PROVES the
-- command was never sent -- in_flight is written first, always.
--
-- THE GUARD IS THE PROOF. The reconciler reads its candidates outside the
-- transaction that resolves them, because resolving means an RCON round trip
-- per row. So a row it read as pending may have been claimed since, and the
-- moment it is claimed "nothing was sent" stops being true. 0 rows means
-- exactly that, and the row belongs to the request that claimed it.
-- name: FailAbandonedOperation :execrows
update bank_ledger
   set state       = 'failed',
       error       = $2,
       resolved_at = now()
 where id = $1 and state = 'pending';

-- ParkForReview closes a row whose outcome could not be established. This is
-- the only state a human has to act on, and the only way marks can be wrong.
--
-- Guarded like the other terminal transitions: a row is only ever parked
-- because its command MAY have landed, which is the in_flight state and no
-- other, and parking must never overwrite a row somebody else has already
-- resolved. 0 rows means they did.
-- name: ParkForReview :execrows
update bank_ledger
   set state       = 'needs_review',
       error       = $2,
       marks_after = $3,
       resolved_at = now()
 where id = $1 and state = 'in_flight';

-- RecordVerifyAttempt counts one observation of an unresolved row and doubles as
-- the reconciler's claim on it: the guard means no row is returned once the row
-- has been resolved by the request path or another replica, so no RCON round
-- trip is spent on it and no attempt is charged against a decided row.
-- name: RecordVerifyAttempt :one
update bank_ledger
   set verify_attempts = verify_attempts + 1
 where id = $1 and state = 'in_flight'
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
