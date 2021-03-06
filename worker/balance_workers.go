package worker

import (
	"context"
	"time"

	"github.com/quintans/eventsourcing/log"
)

type MemberWorkers struct {
	Name    string
	Workers []string
}

type Memberlister interface {
	Name() string
	List(context.Context) ([]MemberWorkers, error)
	Register(context.Context, []string) error
}

type Worker interface {
	Name() string
	IsRunning() bool
	Start(context.Context) bool
	Stop(context.Context)
}

func BalanceWorkers(ctx context.Context, logger log.Logger, member Memberlister, workers []Worker, heartbeat time.Duration) {
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		err := run(ctx, member, workers)
		if err != nil {
			logger.Warnf("Error while balancing partitions: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func run(ctx context.Context, member Memberlister, workers []Worker) error {
	members, err := member.List(ctx)
	if err != nil {
		return err
	}

	// if current member is not in the list, add it to the member count
	present := false
	for _, v := range members {
		if v.Name == member.Name() {
			present = true
			break
		}
	}
	membersCount := len(members)
	if !present {
		membersCount++
	}

	monitorsNo := len(workers)
	workersToAcquire := monitorsNo / membersCount

	// check if all members have the minimum workers. Only after that, any additional can be picked up.
	allHaveMinWorkers := true
	workersInUse := map[string]bool{}
	for _, m := range members {
		// checking if others have min required workers running.
		// This member might be included
		if len(m.Workers) < workersToAcquire {
			allHaveMinWorkers = false
		}
		// map only other members workers
		if m.Name != member.Name() {
			for _, v := range m.Workers {
				workersInUse[v] = true
			}
		}
	}
	// mapping my current workers
	myRunningWorkers := map[string]bool{}
	for _, v := range workers {
		if v.IsRunning() {
			workersInUse[v.Name()] = true
			myRunningWorkers[v.Name()] = true
		}
	}
	// if my current running workers are less, then not all members have the min workers
	if len(myRunningWorkers) < workersToAcquire {
		allHaveMinWorkers = false
	}

	if allHaveMinWorkers && monitorsNo%membersCount != 0 {
		workersToAcquire++
	}

	locks := balance(ctx, workers, workersToAcquire, workersInUse, myRunningWorkers)
	member.Register(ctx, locks)

	return nil
}

func balance(ctx context.Context, workers []Worker, workersToAcquire int, workersInUse, myRunningWorkers map[string]bool) []string {
	running := len(myRunningWorkers)
	if running == workersToAcquire {
		return mapToString(myRunningWorkers)
	}

	for _, v := range workers {
		if running > workersToAcquire {
			if !v.IsRunning() {
				continue
			}

			v.Stop(ctx)
			delete(myRunningWorkers, v.Name())
			running--
		} else {
			if workersInUse[v.Name()] {
				continue
			}

			if v.Start(ctx) {
				myRunningWorkers[v.Name()] = true
				running++
			}
		}
		if running == workersToAcquire {
			break
		}
	}
	return mapToString(myRunningWorkers)
}

func mapToString(m map[string]bool) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	return s
}
