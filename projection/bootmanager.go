package projection

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/quintans/eventstore"
	"github.com/quintans/eventstore/eventid"
	"github.com/quintans/eventstore/player"
	"github.com/quintans/eventstore/repo"
	log "github.com/sirupsen/logrus"
)

type Freezer interface {
	Name() string
	Freeze() bool
	Unfreeze()
}

type Projection interface {
	GetName() string
	GetResumeEventID(ctx context.Context) (string, error)
	Handler(ctx context.Context, e eventstore.Event) error
	GetAggregateTypes() []string
}

type Subscriber interface {
	StartConsumer(ctx context.Context, partition int, resumeToken string, projection Projection) (chan struct{}, error)
	StartNotifier(ctx context.Context, freezer Freezer) error
	GetResumeToken(ctx context.Context, partition int) (string, error)
	FreezeProjection(ctx context.Context, projectionName string) error
	UnfreezeProjection(ctx context.Context, projectionName string) error
}

type EventHandler func(ctx context.Context, e eventstore.Event) error

type BootableManager struct {
	projection  Projection
	subscriber  Subscriber
	replayer    player.Replayer
	repository  player.Repository
	partitionLo int
	partitionHi int

	cancel  context.CancelFunc
	wait    chan struct{}
	release chan struct{}
	closed  bool
	frozen  []chan struct{}
	hasLock bool // acquired the lock
	mu      sync.RWMutex
}

// NewBootableManager creates an instance that manages the lifecycle of a projection that has the capability of being stopped and restarted on demand.
// Arguments:
//   projection: the projection
//   subscriber: handles all interaction with the message queue
//   repository: repository to the events
//   topic: topic from where the events will be consumed
//   partitionLo: first partition number. if zero, partitioning will ignored
//   partitionHi: last partition number. if zero, partitioning will ignored
//   managerTopic: topic used to signal when to stop or start a projection
func NewBootableManager(
	projection Projection,
	subscriber Subscriber,
	repository player.Repository,
	partitionLo, partitionHi int,
) *BootableManager {
	c := make(chan struct{})
	close(c)
	mc := &BootableManager{
		projection:  projection,
		subscriber:  subscriber,
		repository:  repository,
		partitionLo: partitionLo,
		partitionHi: partitionHi,
		wait:        c,
		release:     make(chan struct{}),
	}

	mc.replayer = player.New(repository)

	return mc
}

func (m *BootableManager) Name() string {
	return m.projection.GetName()
}

// Wait block the call if the MonitorableConsumer was put on hold
func (m *BootableManager) Wait() <-chan struct{} {
	m.mu.Lock()
	wait := m.wait
	m.mu.Unlock()

	<-wait

	m.mu.Lock()
	defer m.mu.Unlock()
	m.release = make(chan struct{})
	return m.release
}

// OnBoot action to be executed on boot
func (m *BootableManager) OnBoot(ctx context.Context) error {
	var ctx2 context.Context
	ctx2, m.cancel = context.WithCancel(ctx)
	err := m.boot(ctx2)
	if err != nil {
		m.cancel()
		return err
	}

	err = m.subscriber.StartNotifier(ctx, m)
	if err != nil {
		m.cancel()
		return err
	}

	m.hasLock = true
	return nil
}

func (m *BootableManager) boot(ctx context.Context) error {
	// get the smallest of the latest event ID for each partitioned topic from the DB
	prjEventID, err := m.projection.GetResumeEventID(ctx)
	if err != nil {
		return fmt.Errorf("Could not get last event ID from the projection: %w", err)
	}

	// To avoid the creation of  potential massive buffer size
	// and to ensure that events are not lost, between the switch to the consumer,
	// we execute the fetch in several steps.
	// 1) Process all events from the ES from the begginning - it may be a long operation
	// 2) start the consumer to track new events from now on
	// 3) process any event that may have arrived between the switch
	// 4) start consuming events from the last position
	handler := m.projection.Handler

	aggregateTypes := m.projection.GetAggregateTypes()
	// replay oldest events
	prjEventID, err = eventid.DelayEventID(prjEventID, player.TrailingLag)
	if err != nil {
		return fmt.Errorf("Error delaying the eventID: %w", err)
	}
	log.Printf("Booting %s from '%s'", m.projection.GetName(), prjEventID)

	filter := repo.WithAggregateTypes(aggregateTypes...)
	lastEventID, err := m.replayer.Replay(ctx, handler, prjEventID, filter)
	if err != nil {
		return fmt.Errorf("Could not replay all events (first part): %w", err)
	}

	// grab the last events sequences in the topic (partitioned)
	partitionSize := m.partitionHi - m.partitionLo + 1
	tokens := make([]string, partitionSize)
	for i := 0; i < partitionSize; i++ {
		tokens[i], err = m.subscriber.GetResumeToken(ctx, m.partitionLo+i)
		if err != nil {
			return fmt.Errorf("Could not retrieve resume token: %w", err)
		}
	}

	// consume potential missed events events between the switch to the consumer
	events, err := m.repository.GetEvents(ctx, lastEventID, 0, time.Duration(0), repo.Filter{
		AggregateTypes: aggregateTypes,
	})
	if err != nil {
		return fmt.Errorf("Could not replay all events (second part): %w", err)
	}
	for _, event := range events {
		err = handler(ctx, event)
		if err != nil {
			return fmt.Errorf("Error handling event %+v: %w", event, err)
		}
	}
	// start consuming events from the last available position
	frozen := make([]chan struct{}, partitionSize)
	for i := 0; i < partitionSize; i++ {
		ch, err := m.subscriber.StartConsumer(ctx, m.partitionLo+i, tokens[i], m.projection)
		if err != nil {
			return fmt.Errorf("Unable to start consumer: %w", err)
		}
		frozen[i] = ch
	}

	m.mu.Lock()
	m.frozen = frozen
	m.mu.Unlock()

	return nil
}

func (m *BootableManager) Cancel() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
}

// Freeze blocks calls to Wait() return true if this instance was locked
func (m *BootableManager) Freeze() bool {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.wait = make(chan struct{})
	m.closed = false
	close(m.release)
	m.release = make(chan struct{})

	if m.hasLock {
		// wait for the closing subscriber
		for _, ch := range m.frozen {
			<-ch
		}
	}
	locked := m.hasLock
	m.hasLock = false

	m.mu.Unlock()

	return locked
}

// Unfreeze releases any blocking call to Wait()
// points the cursor to the first available record
func (m *BootableManager) Unfreeze() {
	m.mu.Lock()
	if !m.closed {
		close(m.wait)
		m.closed = true
	}
	m.mu.Unlock()
}

type Action int

const (
	Freeze Action = iota + 1
	Unfreeze
)

type Notification struct {
	Projection string `json:"projection"`
	Action
}
