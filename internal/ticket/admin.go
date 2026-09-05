package ticket

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The state machine, small on purpose.
//
// Reopening is allowed from RESOLVED and CLOSED and requires a reason, because a ticket
// that comes back is the interesting case and "why" is the only part worth keeping.
// Resolving requires a conclusion for the same reason: a RESOLVED ticket with no record
// of what was done is indistinguishable from an abandoned one.
var allowedTransitions = map[State][]State{
	StateOpen:       {StateInProgress, StateClosed},
	StateInProgress: {StateResolved, StateClosed},
	StateResolved:   {StateInProgress, StateClosed},
	StateClosed:     {StateInProgress},
}

func (s State) canBecome(next State) bool {
	for _, allowed := range allowedTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

type Event struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Detail string    `json:"detail,omitempty"`
}

type Filter struct {
	State          State
	Assignee       string
	ConversationID string
	Limit, Offset  int
}

// List returns tickets newest-updated first. The page size is bounded here rather than
// trusted from a query string.
func (s *Store) List(ctx context.Context, f Filter) ([]Ticket, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	const where = `
		WHERE ($1 = '' OR state = $1)
		  AND ($2 = '' OR assignee = $2)
		  AND ($3 = '' OR conversation_id = $3)`

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM support_ticket`+where,
		string(f.State), f.Assignee, f.ConversationID).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.pool.Query(ctx, selectColumns+` FROM support_ticket`+where+`
		ORDER BY updated_at DESC, ticket_number DESC LIMIT $4 OFFSET $5`,
		string(f.State), f.Assignee, f.ConversationID, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []Ticket
	for rows.Next() {
		t, err := scanOne(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

func (s *Store) Get(ctx context.Context, number string) (Ticket, []Event, error) {
	t, err := scanOne(s.pool.QueryRow(ctx, selectColumns+
		` FROM support_ticket WHERE ticket_number = $1`, number))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, nil, ErrNotFound
	}
	if err != nil {
		return Ticket{}, nil, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT at, actor, action, coalesce(detail,'') FROM ticket_event
		 WHERE ticket_number = $1 ORDER BY id`, number)
	if err != nil {
		return Ticket{}, nil, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.Detail); err != nil {
			return Ticket{}, nil, err
		}
		events = append(events, e)
	}
	return t, events, rows.Err()
}

// Update is every admin mutation, because they share one requirement: the change, the
// history entry and the version bump must land together or not at all.
//
// ExpectedVersion is not optional. Two operators with the same ticket open will
// otherwise overwrite each other and the loser sees no error -- the shape of bug that
// only appears once two people use the thing.
type Update struct {
	Actor           string
	ExpectedVersion int

	State      State   // optional
	Assignee   *string // optional; a pointer so "unassign" differs from "leave alone"
	Resolution string  // required when moving to RESOLVED
	Note       string  // free text, recorded either way
	Reason     string  // required when reopening
}

func (s *Store) Update(ctx context.Context, number string, u Update) (Ticket, error) {
	if strings.TrimSpace(u.Actor) == "" {
		return Ticket{}, errors.New("an update needs an actor")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, err
	}
	defer tx.Rollback(ctx)

	current, err := scanOne(tx.QueryRow(ctx, selectColumns+
		` FROM support_ticket WHERE ticket_number = $1 FOR UPDATE`, number))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	if current.Version != u.ExpectedVersion {
		return Ticket{}, fmt.Errorf("%w: it is at version %d, you have %d",
			ErrConflict, current.Version, u.ExpectedVersion)
	}

	next := current
	if u.Assignee != nil {
		next.Assignee = strings.TrimSpace(*u.Assignee)
		action := "assigned"
		detail := next.Assignee
		if next.Assignee == "" {
			action, detail = "unassigned", ""
		}
		if err := appendEvent(ctx, tx, number, u.Actor, action, detail); err != nil {
			return Ticket{}, err
		}
	}

	if u.State != "" && u.State != current.State {
		if !current.State.canBecome(u.State) {
			return Ticket{}, fmt.Errorf("cannot move a ticket from %s to %s",
				current.State, u.State)
		}
		reopening := (current.State == StateResolved || current.State == StateClosed) &&
			u.State == StateInProgress
		if reopening && strings.TrimSpace(u.Reason) == "" {
			return Ticket{}, errors.New("reopening a ticket needs a reason")
		}
		if u.State == StateResolved && strings.TrimSpace(u.Resolution) == "" {
			return Ticket{}, errors.New("resolving a ticket needs a conclusion")
		}
		next.State = u.State
		switch {
		case reopening:
			// A reopen disputes the conclusion, so the row must stop asserting one.
			// Carrying it forward leaves a ticket that is IN_PROGRESS and also claims
			// to have been concluded -- and the page pre-fills the resolution box from
			// the row, so an operator reopening a ticket resubmits the old conclusion
			// without touching it. Nothing is lost: the state change that resolved it
			// carries the text in the history.
			next.Resolution = ""
		case u.Resolution != "":
			next.Resolution = u.Resolution
		}
		detail := u.Reason
		if detail == "" {
			detail = u.Resolution
		}
		if err := appendEvent(ctx, tx, number, u.Actor,
			"state "+string(current.State)+" -> "+string(u.State), detail); err != nil {
			return Ticket{}, err
		}
	}

	if strings.TrimSpace(u.Note) != "" {
		if err := appendEvent(ctx, tx, number, u.Actor, "note", u.Note); err != nil {
			return Ticket{}, err
		}
	}

	updated, err := scanOne(tx.QueryRow(ctx, `
		UPDATE support_ticket
		SET state = $2, assignee = NULLIF($3,''), resolution = NULLIF($4,''),
		    version = version + 1, updated_at = now()
		WHERE ticket_number = $1 AND version = $5
		RETURNING `+columns,
		number, string(next.State), next.Assignee, next.Resolution, u.ExpectedVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		// Someone committed between the SELECT FOR UPDATE and here. Not reachable with
		// the row lock held, and reported rather than ignored if it ever is.
		return Ticket{}, ErrConflict
	}
	if err != nil {
		return Ticket{}, err
	}
	return updated, tx.Commit(ctx)
}

// Counts backs the overview: how many tickets sit in each state.
func (s *Store) Counts(ctx context.Context) (map[State]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT state, count(*) FROM support_ticket GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[State]int{}
	for rows.Next() {
		var st State
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}
