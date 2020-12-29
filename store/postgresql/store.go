package postgresql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/quintans/eventstore"
	"github.com/quintans/eventstore/common"
	"github.com/quintans/eventstore/store"
)

const (
	pgUniqueViolation = "23505"
)

// Event is the event data stored in the database
type Event struct {
	ID               string    `db:"id"`
	AggregateID      string    `db:"aggregate_id"`
	AggregateVersion uint32    `db:"aggregate_version"`
	AggregateType    string    `db:"aggregate_type"`
	Kind             string    `db:"kind"`
	Body             []byte    `db:"body"`
	IdempotencyKey   string    `db:"idempotency_key"`
	Labels           []byte    `db:"labels"`
	CreatedAt        time.Time `db:"created_at"`
}

type Snapshot struct {
	ID               string    `db:"id,omitempty"`
	AggregateID      string    `db:"aggregate_id,omitempty"`
	AggregateVersion uint32    `db:"aggregate_version,omitempty"`
	AggregateType    string    `db:"aggregate_type,omitempty"`
	Body             []byte    `db:"body,omitempty"`
	CreatedAt        time.Time `db:"created_at,omitempty"`
}

var _ eventstore.EsRepository = (*EsRepository)(nil)

type StoreOption func(*EsRepository)

type ProjectorFactory func(*sql.Tx) store.Projector

func ProjectorFactoryOption(fn ProjectorFactory) StoreOption {
	return func(r *EsRepository) {
		r.projectorFactory = fn
	}
}

type EsRepository struct {
	db               *sqlx.DB
	projectorFactory ProjectorFactory
}

func NewStore(dburl string, options ...StoreOption) (*EsRepository, error) {
	db, err := sql.Open("postgres", dburl)
	if err != nil {
		return nil, err
	}
	return NewStoreDB(db, options...)
}

// NewStoreDB creates a new instance of PgEsRepository
func NewStoreDB(db *sql.DB, options ...StoreOption) (*EsRepository, error) {
	dbx := sqlx.NewDb(db, "postgres")
	r := &EsRepository{
		db: dbx,
	}

	for _, o := range options {
		o(r)
	}

	return r, nil
}

func (r *EsRepository) SaveEvent(ctx context.Context, eRec eventstore.EventRecord) (string, uint32, error) {
	labels, err := json.Marshal(eRec.Labels)
	if err != nil {
		return "", 0, err
	}

	version := eRec.Version
	var id string
	err = r.withTx(ctx, func(c context.Context, tx *sql.Tx) error {
		var projector store.Projector
		if r.projectorFactory != nil {
			projector = r.projectorFactory(tx)
		}
		for _, e := range eRec.Details {
			version++
			id = common.NewEventID(eRec.CreatedAt, eRec.AggregateID, version)
			h := common.Hash(eRec.AggregateID)
			_, err = tx.ExecContext(ctx,
				`INSERT INTO events (id, aggregate_id, aggregate_version, aggregate_type, kind, body, idempotency_key, labels, created_at, aggregate_id_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				id, eRec.AggregateID, version, eRec.AggregateType, e.Kind, e.Body, eRec.IdempotencyKey, labels, eRec.CreatedAt, h)

			if err != nil {
				if isPgDup(err) {
					return eventstore.ErrConcurrentModification
				}
				return fmt.Errorf("Unable to insert event: %w", err)
			}

			if projector != nil {
				evt := eventstore.Event{
					ID:               id,
					AggregateID:      eRec.AggregateID,
					AggregateVersion: version,
					AggregateType:    eRec.AggregateType,
					Kind:             e.Kind,
					Body:             e.Body,
					Labels:           eRec.Labels,
					CreatedAt:        eRec.CreatedAt,
				}
				projector.Project(evt)
			}
		}

		return nil
	})
	if err != nil {
		return "", 0, err
	}

	return id, version, nil
}

func isPgDup(err error) bool {
	if pgerr, ok := err.(*pq.Error); ok {
		if pgerr.Code == pgUniqueViolation {
			return true
		}
	}
	return false
}

func (r *EsRepository) GetSnapshot(ctx context.Context, aggregateID string) (eventstore.Snapshot, error) {
	snap := Snapshot{}
	if err := r.db.GetContext(ctx, &snap, "SELECT * FROM snapshots WHERE aggregate_id = $1 ORDER BY id DESC LIMIT 1", aggregateID); err != nil {
		if err == sql.ErrNoRows {
			return eventstore.Snapshot{}, nil
		}
		return eventstore.Snapshot{}, fmt.Errorf("Unable to get snapshot for aggregate '%s': %w", aggregateID, err)
	}
	return eventstore.Snapshot{
		ID:               snap.ID,
		AggregateID:      snap.AggregateID,
		AggregateVersion: snap.AggregateVersion,
		AggregateType:    snap.AggregateType,
		Body:             snap.Body,
		CreatedAt:        snap.CreatedAt,
	}, nil
}

func (r *EsRepository) SaveSnapshot(ctx context.Context, snapshot eventstore.Snapshot) error {
	s := Snapshot{
		ID:               snapshot.ID,
		AggregateID:      snapshot.AggregateID,
		AggregateVersion: snapshot.AggregateVersion,
		AggregateType:    snapshot.AggregateType,
		Body:             snapshot.Body,
		CreatedAt:        snapshot.CreatedAt,
	}
	_, err := r.db.NamedExecContext(ctx,
		`INSERT INTO snapshots (id, aggregate_id, aggregate_version, aggregate_type, body, created_at)
	     VALUES (:id, :aggregate_id, :aggregate_version, :aggregate_type, :body, :created_at)`, s)

	return err
}

func (r *EsRepository) GetAggregateEvents(ctx context.Context, aggregateID string, snapVersion int) ([]eventstore.Event, error) {
	var query bytes.Buffer
	query.WriteString("SELECT * FROM events e WHERE e.aggregate_id = $1")
	args := []interface{}{aggregateID}
	if snapVersion > -1 {
		query.WriteString(" AND e.aggregate_version > $2")
		args = append(args, snapVersion)
	}
	query.WriteString(" ORDER BY aggregate_version ASC")

	events, err := r.queryEvents(ctx, query.String(), "", args...)
	if err != nil {
		return nil, fmt.Errorf("Unable to get events for Aggregate '%s': %w", aggregateID, err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("Aggregate '%s' was not found: %w", aggregateID, err)
	}

	return events, nil
}

func (r *EsRepository) withTx(ctx context.Context, fn func(context.Context, *sql.Tx) error) (err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			panic(r)
		}
		if err != nil {
			tx.Rollback()
		}
	}()
	err = fn(ctx, tx)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *EsRepository) HasIdempotencyKey(ctx context.Context, aggregateID, idempotencyKey string) (bool, error) {
	var exists int
	err := r.db.GetContext(ctx, &exists, `SELECT EXISTS(SELECT 1 FROM events WHERE idempotency_key=$1 AND aggregate_type=$2) AS "EXISTS"`, idempotencyKey, aggregateID)
	if err != nil {
		return false, fmt.Errorf("Unable to verify the existence of the idempotency key: %w", err)
	}
	return exists != 0, nil
}

func (r *EsRepository) Forget(ctx context.Context, request eventstore.ForgetRequest, forget func(kind string, body []byte) ([]byte, error)) error {
	// When Forget() is called, the aggregate is no longer used, therefore if it fails, it can be called again.

	// Forget events
	events, err := r.queryEvents(ctx, "SELECT * FROM events WHERE aggregate_id = $1 AND kind = $2", "", request.AggregateID, request.EventKind)
	if err != nil {
		return fmt.Errorf("Unable to get events for Aggregate '%s' and event kind '%s': %w", request.AggregateID, request.EventKind, err)
	}

	for _, evt := range events {
		body, err := forget(evt.Kind, evt.Body)
		if err != nil {
			return err
		}
		_, err = r.db.ExecContext(ctx, "UPDATE events SET body = $1 WHERE ID = $2", body, evt.ID)
		if err != nil {
			return fmt.Errorf("Unable to forget event ID %s: %w", evt.ID, err)
		}
	}

	// forget snapshots
	snaps := []Snapshot{}
	if err := r.db.SelectContext(ctx, &snaps, "SELECT * FROM snapshots WHERE aggregate_id = $1", request.AggregateID); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("Unable to get snapshot for aggregate '%s': %w", request.AggregateID, err)
	}

	for _, snap := range snaps {
		body, err := forget(snap.AggregateType, snap.Body)
		if err != nil {
			return err
		}
		_, err = r.db.ExecContext(ctx, "UPDATE snapshots SET body = $1 WHERE ID = $2", body, snap.ID)
		if err != nil {
			return fmt.Errorf("Unable to forget snapshot ID %s: %w", snap.ID, err)
		}
	}

	return nil
}

func (r *EsRepository) GetLastEventID(ctx context.Context, trailingLag time.Duration, filter store.Filter) (string, error) {
	var query bytes.Buffer
	query.WriteString("SELECT * FROM events ")
	args := []interface{}{}
	if trailingLag != time.Duration(0) {
		safetyMargin := time.Now().UTC().Add(-trailingLag)
		args = append(args, safetyMargin)
		query.WriteString("created_at <= $1 ")
	}
	args = buildFilter(filter, &query, args)
	query.WriteString(" ORDER BY id DESC LIMIT 1")
	var eventID string
	if err := r.db.GetContext(ctx, &eventID, query.String(), args...); err != nil {
		if err != sql.ErrNoRows {
			return "", fmt.Errorf("Unable to get the last event ID: %w", err)
		}
	}
	return eventID, nil
}

func (r *EsRepository) GetEvents(ctx context.Context, afterEventID string, batchSize int, trailingLag time.Duration, filter store.Filter) ([]eventstore.Event, error) {
	args := []interface{}{afterEventID}
	var query bytes.Buffer
	query.WriteString("SELECT * FROM events WHERE id > $1 ")
	if trailingLag != time.Duration(0) {
		safetyMargin := time.Now().UTC().Add(-trailingLag)
		args = append(args, safetyMargin)
		query.WriteString("AND created_at <= $2 ")
	}
	args = buildFilter(filter, &query, args)
	query.WriteString(" ORDER BY id ASC")
	if batchSize > 0 {
		query.WriteString(" LIMIT ")
		query.WriteString(strconv.Itoa(batchSize))
	}

	rows, err := r.queryEvents(ctx, query.String(), afterEventID, args...)
	if err != nil {
		return nil, fmt.Errorf("Unable to get events after '%s' for filter %+v: %w", afterEventID, filter, err)
	}
	return rows, nil
}

func buildFilter(filter store.Filter, query *bytes.Buffer, args []interface{}) []interface{} {
	if len(filter.AggregateTypes) > 0 {
		query.WriteString(" AND (")
		for k, v := range filter.AggregateTypes {
			if k > 0 {
				query.WriteString(" OR ")
			}
			args = append(args, v)
			query.WriteString(fmt.Sprintf("aggregate_type = $%d", len(args)))
		}
		query.WriteString(")")
	}

	if filter.Partitions > 0 {
		size := len(args)
		if filter.PartitionsLow == filter.PartitionsHi {
			args = append(args, filter.Partitions, filter.PartitionsLow-1)
			query.WriteString(fmt.Sprintf(" AND MOD(aggregate_id_hash, $%d) = $%d", size+1, size+2))
		} else {
			args = append(args, filter.Partitions, filter.PartitionsLow-1, filter.PartitionsHi-1)
			query.WriteString(fmt.Sprintf(" AND MOD(aggregate_id_hash, $%d) BETWEEN $%d AND $%d", size+1, size+2, size+4))
		}
	}

	if len(filter.Labels) > 0 {
		for k, values := range filter.Labels {
			k = escape(k)
			query.WriteString(" AND (")
			for idx, v := range values {
				if idx > 0 {
					query.WriteString(" OR ")
				}
				v = escape(v)
				query.WriteString(fmt.Sprintf(`labels  @> '{"%s": "%s"}'`, k, v))
				query.WriteString(")")
			}
		}
	}
	return args
}

func escape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func (r *EsRepository) queryEvents(ctx context.Context, query string, afterEventID string, args ...interface{}) ([]eventstore.Event, error) {
	rows, err := r.db.QueryxContext(ctx, query, args...)
	if err != nil {
		if err == sql.ErrNoRows {
			return []eventstore.Event{}, nil
		}
		return nil, err
	}
	events := []eventstore.Event{}
	for rows.Next() {
		pg := Event{}
		err := rows.StructScan(&pg)
		if err != nil {
			return nil, fmt.Errorf("Unable to scan to struct: %w", err)
		}
		labels := map[string]interface{}{}
		err = json.Unmarshal(pg.Labels, &labels)
		if err != nil {
			return nil, fmt.Errorf("Unable to unmarshal labels to map: %w", err)
		}

		events = append(events, eventstore.Event{
			ID:               pg.ID,
			AggregateID:      pg.AggregateID,
			AggregateVersion: pg.AggregateVersion,
			AggregateType:    pg.AggregateType,
			Kind:             pg.Kind,
			Body:             pg.Body,
			Labels:           labels,
			CreatedAt:        pg.CreatedAt,
		})
	}
	return events, nil
}