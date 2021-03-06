package pg

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/quintans/eventsourcing"
	"github.com/quintans/eventsourcing/player"
	"github.com/quintans/eventsourcing/sink"
	"github.com/quintans/eventsourcing/store/postgresql"
	"github.com/quintans/eventsourcing/test"
)

func TestPgListener(t *testing.T) {
	dbConfig, tearDown, err := setup()
	require.NoError(t, err)
	defer tearDown()

	repository, err := postgresql.NewStore(dbConfig.Url())
	require.NoError(t, err)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	s := test.NewMockSink(1)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := feeding(ctx, dbConfig, repository, s)

	es := eventsourcing.NewEventStore(repository, test.AggregateFactory{}, eventsourcing.WithSnapshotThreshold(3))

	id := uuid.New()
	acc := test.CreateAccount("Paulo", id, 100)
	acc.Deposit(10)
	acc.Deposit(20)
	err = es.Save(ctx, acc)
	require.NoError(t, err)

	// giving time for the snapshots to write
	time.Sleep(100 * time.Millisecond)
	events := s.GetEvents()
	assert.Equal(t, 3, len(events), "event size")
	assert.Equal(t, "AccountCreated", events[0].Kind.String())
	assert.Equal(t, "MoneyDeposited", events[1].Kind.String())
	assert.Equal(t, "MoneyDeposited", events[2].Kind.String())

	cancel()
	require.NoError(t, <-errCh, "Error feeding")
}

func feeding(ctx context.Context, dbConfig DBConfig, repository player.Repository, sinker sink.Sinker) chan error {
	errCh := make(chan error, 1)
	done := make(chan struct{})
	listener := postgresql.NewFeedListenNotify(logger, dbConfig.ReplicationUrl(), repository, "events_channel")
	go func() {
		close(done)
		err := listener.Feed(ctx, sinker)
		if err != nil && !errors.Is(err, context.Canceled) {
			errCh <- err
		} else {
			errCh <- nil
		}
	}()
	<-done
	return errCh
}
