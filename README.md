# Event Store
a simple implementation of an event store using only a database

This an exercise on how I could implement a event store and how this could be used with CQRS.

## Introduction

The goal of this project is to implement an event store and how this event store could be used with the Event Sourcing + CQRS Architecture pattern.

### Event Bus

The main challenge is, how to store events into a database and then propagate it to a event bus without losing any event.

In most of the implementations that I have seen, some sort of event bus is used to deliver the domain events to external consumers, without considering that it is not possible to write into a database and publish to a event bus in a transaction. Many things can go wrong, like for example, after writing to the database we fail to write into the event event bus. Even if it fails, there is no guarantee that it wasn't published.

### Polling

The solution for this challenge is to store events with a incremental key. With this we can then poll for events after the last polled event.
Most databases have the capability of generating more or less incremental keys, that can be used for events IDs.

> What is important is that for a given aggregate the events IDs are ordered, so strict ordering between aggregates is not important.

A poller process can then poll the database for new events. The events can be published to a event bus or consumed directly.

> Consuming events is discuss in more detail below, in CQRS section. 

But there is catch. Consider two concurrent transactions. One acquires the ID 100 and the other the ID 101. If the one with ID 101 is faster to finish the transaction, it will show up first in a query than the one with ID 100.
Unless we take some precautions, records will not always become visible in the expected order.

If we have a polling process that relies on this number to determine from where to start polling, it could miss the last added record, and this could lead to events not being tracked.

But there is a solution. We can only retrieving events older than X milliseconds, to allow for the concurrent transactions to complete.

An example of how this could be done in PostgreSQL:

```sql
SELECT * FROM events 
WHERE id >= $1
AND created_at <= NOW()::TIMESTAMP - INTERVAL'1 seconds'
ORDER BY id ASC
LIMIT 100
```

This polling strategy can be used both with SQL and NoSQL databases, like Postgresql, Cockroach or MongoDB, to name a few.

As a side note I would like to say that I went for the polling strategy because it fits well for SQL and NoSQL generic databases and it is simple to understand and straight forward to implement.
There are other solutions that might work better if we took advantage of specific features that some database vendors provide. 

### IDs

Since SQL databases usually use a number for incremental keys, and NoSQL databases use strings, I opted to use strings as the event ID,
specifically the ones generated by [xid](https://github.com/rs/xid). These IDs are ordered in time, with a millisecond precision.

It is important to use the tracking query latency, mentioned above, to compensate server clock differences.

### NoSQL

When we interact with an aggregate, several events may be created. 

In the current implementation, a PostgreSQL is used, so we create a record per event and used a transaction to guarantee consistency.

Depending of the used database a different approach must be used.
For example, for document NoSQL databases like MongoDB, the solution is to use one document with the array of events inside.
The only requirement is that the database needs to provide unique constraints on multiple fields (aggregate_id, version).

The record would be something like:

`{ _id = 1, aggregate_id = 1, version = 1, events = [ { … }, { … }, { … }, { … } ] }`


### Snapshots

I will also use the memento pattern, to take snapshots of the current state, every X events.

Snapshots is a technique used to improve the performance of the event store, when retrieving an aggregate, but they don't play any part in keeping the consistency of the event store, therefore if we sporadically fail to save a snapshot, it is not a problem, so they can be saved in a separate transaction and go routine.

### Idempotency

When saving an aggregate, we have the option to supply an idempotent key. Later, we can check the presence of the idempotency key, to see if we are repeating an action. This can be useful when used in process manager reactors.

In most the examples I've seen, about implementing a process manager, it is not clear what is the value in breaking into several subscribers to handle every step of the process, and I think this is because they only consider the happy paths.
If the process is only considering the happy path, there is no advantage in having several subscribers, by the contrary.
If we introduce compensation actions, it becomes clear that there is an advantage in using a several subscribers to manage the "transaction" involving multiple aggregates.

In the following example I exemplify a money transfer with rollback actions, leveraging idempotent keys.

Here, Withdraw and Deposit need to be idempotent, but setting the transfer state does not. The latter is idempotent action while the former is not. 

> I don't see the need to use command handlers in the following examples

```go
func NewTransferReactor(es EventStore) {
    // ...
	l := NewPoller(es)
	cancel, err := l.Handle(ctx, func(c context.Context, e Event) {
        switch e.Kind {
        case "TransferStarted":
            OnTransferStarted(c, es, e)
        case "MoneyWithdrawn":
            OnMoneyWithdrawn(c, es, e)
        case "MoneyDeposited":
            OnMoneyDeposited(c, es, e)
        case "TransferFailedToDeposit":
            OnTransferFailedToDeposit(c, es, e)
        }
    })
    // ...
}

func OnTransferStarted(ctx context.Context, es EventStore, e Event) {
    event = NewTransferStarted(e)
    transfer := NewTransfer()
    es.GetByID(ctx, event.Transaction, &transfer)
    if !transfer.IsRunning() {
        return
    }
    
    // event.Transaction is the idempotent key for the account withdrawal
    exists, _ := es.HasIdempotencyKey(ctx, event.FromAccount, event.Transaction)
    if !exists {
        account := NewAccount()
        es.GetByID(ctx, event.FromAccount, &account)
        if ok := account.Withdraw(event.Amount, event.Transaction); !ok {
            transfer.FailedWithdraw("Not Enough Funds")
            es.Save(ctx, transfer, Options{})
            return
        }
        es.Save(ctx, account, Options{
            IdempotencyKey: event.Transaction,
        })
    }
}

func OnMoneyWithdrawn(ctx context.Context, es EventStore, e Event) {
    event := NewMoneyWithdrawnEvent(e)
    transfer := NewTransfer()
    es.GetByID(ctx, event.Transaction, &transfer)
    if !transfer.IsRunning() {
        return
    }
    
    transfer.Debited()
    es.Save(ctx, transfer, Options{})

    exists, _ = es.HasIdempotencyKey(ctx, transfer.ToAccount, transfer.Transaction)
    if !exists {
        account := NewAccount()
        es.GetByID(ctx, transfer.ToAccount, &account)
        if ok := account.Deposit(transfer.Amount, transfer.Transaction); !ok {
            transfer.FailedDeposit("Some Reason")
            es.Save(ctx, transfer, Options{})
            return
        }
        es.Save(ctx, account, Options{
            IdempotencyKey: transfer.Transaction,
        })
    }
}

func OnMoneyDeposited(ctx context.Context, es EventStore, e Event) {
    event := NewMoneyDepositedEvent(e)

    transfer = NewTransfer()
    es.GetByID(ctx, event.Transaction, &transfer)

    transfer.Credited()
    es.Save(ctx, transfer, Options{
        IdempotencyKey: idempotentKey,
    })
}

func OnTransferFailedToDeposit(ctx context.Context, es EventStore, e Event) {
    event := NewTransferFailedToDepositEvent(e)

    idempotentKey := event.Transaction + "/refund"
    exists, _ = es.HasIdempotencyKey(ctx, event.FromAccount, idempotentKey)
    if !exists {
        account := NewAccount()
        es.GetByID(ctx, event.FromAccount, &account)
        account.Refund(event.Amount, event.Transaction)
        es.Save(ctx, account, Options{
            IdempotencyKey: idempotentKey,
        })
    }
}
```

## Command Query Responsibility Segregation (CQRS) + Event Sourcing

An event store is where we store the events of an application that follows the event sourcing architecture pattern.
This pattern essentially is modelling the changes to the application as a series of events. The state of the application, at a given point in time, can always be reconstructed by replaying the events from the begging of time until that point in time.

CQRS is an application architecture pattern often used with event sourcing.

### Simple use

By using this library player, a simple architecture can be achieved. The player polls the database for new events at a regular interval (should be less than 1 second), for events after a given event id and fans out to registered consumers. So we have one player serving multiple consumers. Consumer can be stopped and restart on demand.
The projectors should take care of persisting the last handled event, so that in the case of a restart it will pick from the last known event.
It is also important to note that for each projectors, can only be working in one instance at a given time, to guarantee that the events are processed in order (if and event is not processed in order we might end up with corrupt data).

![CQRS](cqrs-es-simple.png)

#### Replay
Replaying all the events fo a projection is very easy to achieve. 
1) Stop the respective consumer
2) retrieve all the events using the player
3) Reattach the consumer to lock to a buffer position
4) retrieve any event from the last position returned in 2)
5) resume consuming

### Alternative (Deprecated)

> I no longer see this approach as an advantage, since now we can have a poller serving multiple consumers.

Another approach is to have an event bus after the data store poller, and let the event bus deliver the events to the projectors, as depicted in the following picture:

![CQRS](cqrs-es.png)

The data store poller polls the database, and writes the events into the event bus.
If it fails to write into the event bus, it queries the event bus for the last message and start polling the database from there.
If the poller service restarts, it will do the same as before, querying the event bus for the last message.
> If it is not possible to get the last published message to the event bus, we can store it in a database.
> Writing repeated messages to the event bus is not a concern, since the used event bus must guarantee `at least once` delivery. It is the job of the projector to be idempotent, discarding repeated messages.

On the projection side it is pretty much the same as in the Simple use, but now we would store the last position in the event bus, so that in the event of a restart, we would know from where to replay the messages.
Improving even further, we could lift the restriction of a projector single instance by using a message bus with an ordered keyed partition, like Apache Kafka. This would avoid different instances handle events for the same aggregate, because the same projector instance would be used for the same aggregate.

Depending on the rate of events being written in the event store, the poller may not be able to keep up and becomes a bottleneck.
When this happens we need to create more polling services that don't overlap when polling event.
Overlapping can be avoided by filtering over metadata.
What this metadata can be and how it is stored will depend in your business case.
A good example is to have a poller per set of aggregates types of per aggregate type.
As an implementation example, for a very broad spectrum of problem, events can be stored with with generic labels, that in turn can be used to filter the events. Each poller would then be sending events into its own event bus topic.

#### Replay

Considering that the event bus should have a limited message retention window, replaying messages from a certain point in time can be achieved in the following manner:
1) get the position of the last message from the event bus
2) consume events from the event store until we reach the event matching the previous event bus position
3) resume listening the event bus from the position of 1)  

### GDPR

According to the GDPR rules, we must completely remove the information that can identify a user. It is not enough to make the information unreadable, for example, by deleting encryption keys.

This means that the data stored in the data store has to change, going against the rule that an event store should only be an append only "log".

Regarding the event-bus, this will not be a problem if we consider a limited retention window for messages (we have 30 days to comply with the GDPR).

```go
es.Forget(ctx, ForgetRequest{
    AggregateID: id,
    AggregateFields: []string{"owner"},
    Events: []EventKind{
        {
            Kind:   "OwnerUpdated",
            Fields: []string{"owner"},
        },
    },
})
```

## gRPC codegen
```sh
./codegen.sh ./api/proto/*.proto
```