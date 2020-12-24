package nv9

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	address "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/rt"
	cid "github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"

	builtin2 "github.com/filecoin-project/specs-actors/v2/actors/builtin"
	states2 "github.com/filecoin-project/specs-actors/v2/actors/states"

	builtin3 "github.com/filecoin-project/specs-actors/v3/actors/builtin"
	states3 "github.com/filecoin-project/specs-actors/v3/actors/states"
	adt3 "github.com/filecoin-project/specs-actors/v3/actors/util/adt"
)

// Config parameterizes a state tree migration
type Config struct {
	// Number of migration worker goroutines to run.
	// More workers enables higher CPU utilization doing migration computations (including state encoding)
	MaxWorkers uint
	// Capacity of the queue of jobs available to workers (zero for unbuffered).
	// A queue length of hundreds to thousands improves throughput at the cost of memory.
	JobQueueSize uint
	// Capacity of the queue receiving migration results from workers, for persisting (zero for unbuffered).
	// A queue length of tens to hundreds improves throughput at the cost of memory.
	ResultQueueSize uint
	// Time between progress logs to emit.
	// Zero (the default) results in no progress logs.
	ProgressLogPeriod time.Duration
}

type StateMigrationInput struct {
	address    address.Address // actor's address
	balance    abi.TokenAmount // actor's balance
	head       cid.Cid         // actor's state head CID
	priorEpoch abi.ChainEpoch  // epoch of last state transition prior to migration
}

type StateMigrationResult struct {
	NewCodeCID cid.Cid
	NewHead    cid.Cid
}

type StateMigration interface {
	// Loads an actor's state from an input store and writes new state to an output store.
	// Returns the new state head CID.
	MigrateState(ctx context.Context, store cbor.IpldStore, input StateMigrationInput) (result *StateMigrationResult, err error)
}

// Migrator which preserves the head CID and provides a fixed result code CID.
type nilMigrator struct {
	OutCodeCID cid.Cid
}

func (n nilMigrator) MigrateState(_ context.Context, _ cbor.IpldStore, in StateMigrationInput) (*StateMigrationResult, error) {
	return &StateMigrationResult{
		NewCodeCID: n.OutCodeCID,
		NewHead:    in.head,
	}, nil
}

type Logger interface {
	// This is the same logging interface provided by the Runtime.
	Log(level rt.LogLevel, msg string, args ...interface{})
}

// Migrates the filecoin state tree starting from the global state tree and upgrading all actor state.
// The store must support concurrent writes (even if the configured worker count is 1).
func MigrateStateTree(ctx context.Context, store cbor.IpldStore, actorsRootIn cid.Cid, priorEpoch abi.ChainEpoch, cfg Config, log Logger) (cid.Cid, error) {
	if cfg.MaxWorkers <= 0 {
		return cid.Undef, xerrors.Errorf("invalid migration config with %d workers", cfg.MaxWorkers)
	}

	// Maps prior version code CIDs to migration functions.
	var migrations = map[cid.Cid]StateMigration{
		builtin2.AccountActorCodeID:          nilMigrator{builtin3.AccountActorCodeID},
		builtin2.CronActorCodeID:             nilMigrator{builtin3.CronActorCodeID},
		builtin2.InitActorCodeID:             initMigrator{},
		builtin2.MultisigActorCodeID:         multisigMigrator{},
		builtin2.PaymentChannelActorCodeID:   paychMigrator{},
		builtin2.RewardActorCodeID:           nilMigrator{builtin3.RewardActorCodeID},
		builtin2.StorageMarketActorCodeID:    marketMigrator{},
		builtin2.StorageMinerActorCodeID:     minerMigrator{},
		builtin2.StoragePowerActorCodeID:     powerMigrator{},
		builtin2.SystemActorCodeID:           nilMigrator{builtin3.SystemActorCodeID},
		builtin2.VerifiedRegistryActorCodeID: verifregMigrator{},
	}
	// Set of prior version code CIDs for actors to defer during iteration, for explicit migration afterwards.
	var deferredCodeIDs = map[cid.Cid]struct{}{
		// None
	}
	if len(migrations)+len(deferredCodeIDs) != 11 {
		panic(fmt.Sprintf("incomplete migration specification with %d code CIDs", len(migrations)))
	}
	startTime := time.Now()

	// Load input and output state trees
	adtStore := adt3.WrapStore(ctx, store)
	actorsIn, err := states2.LoadTree(adtStore, actorsRootIn)
	if err != nil {
		return cid.Undef, err
	}
	actorsOut, err := states3.NewTree(adtStore)
	if err != nil {
		return cid.Undef, err
	}

	// Setup synchronization
	grp, ctx := errgroup.WithContext(ctx)
	// Input and output queues for workers.
	inputCh := make(chan *migrationInput, cfg.JobQueueSize)
	resultCh := make(chan *migrationResult, cfg.ResultQueueSize)
	// Atomically-modified counters for logging progress
	var jobCount uint32
	var doneCount uint32

	// Iterate all actors in old state root to generate migration inputs for each non-deferred actor.
	grp.Go(func() error {
		defer close(inputCh)
		log.Log(rt.INFO, "Creating migration jobs for tree %s", actorsRootIn)
		if err = actorsIn.ForEach(func(addr address.Address, actorIn *states2.Actor) error {
			if _, ok := deferredCodeIDs[actorIn.Code]; ok {
				return nil // Deferred for explicit migration later.
			}
			nextInput := &migrationInput{
				Address:        addr,
				Actor:          *actorIn, // Must take a copy, the pointer is not stable.
				StateMigration: migrations[actorIn.Code],
			}
			select {
			case inputCh <- nextInput:
			case <-ctx.Done():
				return ctx.Err()
			}
			atomic.AddUint32(&jobCount, 1)
			return nil
		}); err != nil {
			return err
		}
		log.Log(rt.INFO, "Done creating %d migration jobs for tree %s after %v", jobCount, actorsRootIn, time.Since(startTime))
		return nil
	})

	// Worker threads run migrations on inputs.
	var workerWg sync.WaitGroup
	for i := uint(0); i < cfg.MaxWorkers; i++ {
		workerWg.Add(1)
		workerId := i
		grp.Go(func() error {
			defer workerWg.Done()
			for input := range inputCh {
				result, err := migrateOneActor(ctx, store, input, priorEpoch)
				if err != nil {
					return err
				}
				select {
				case resultCh <- result:
				case <-ctx.Done():
					return ctx.Err()
				}
				atomic.AddUint32(&doneCount, 1)
			}
			log.Log(rt.INFO, "Worker %d done", workerId)
			return nil
		})
	}
	log.Log(rt.INFO, "Started %d workers", cfg.MaxWorkers)

	// Monitor the job queue. This non-critical goroutine is outside the errgroup and exits when
	// workersFinished is closed, or the context done.
	workersFinished := make(chan struct{}) // Closed when waitgroup is emptied.
	if cfg.ProgressLogPeriod > 0 {
		go func() {
			defer log.Log(rt.DEBUG, "Job queue monitor done")
			for {
				select {
				case <-time.After(cfg.ProgressLogPeriod):
					jobsNow := jobCount // Snapshot values to avoid incorrect-looking arithmetic if they change.
					doneNow := doneCount
					pendingNow := jobsNow - doneNow
					elapsed := time.Since(startTime)
					rate := float64(doneNow) / elapsed.Seconds()
					log.Log(rt.INFO, "%d jobs created, %d done, %d pending after %v (%.0f/s)",
						jobsNow, doneNow, pendingNow, elapsed, rate)
				case <-workersFinished:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Close result channel when workers are done sending to it.
	grp.Go(func() error {
		workerWg.Wait()
		close(resultCh)
		close(workersFinished)
		log.Log(rt.INFO, "All workers done after %v", time.Since(startTime))
		return nil
	})

	// Insert migrated records in output state tree and accumulators.
	grp.Go(func() error {
		log.Log(rt.INFO, "Result writer started")
		resultCount := 0
		for result := range resultCh {
			if err := actorsOut.SetActor(result.Address, &result.Actor); err != nil {
				return err
			}
			resultCount++
		}
		log.Log(rt.INFO, "Result writer wrote %d results to state tree after %v", resultCount, time.Since(startTime))
		return nil
	})

	if err := grp.Wait(); err != nil {
		return cid.Undef, err
	}

	// Perform any deferred migrations explicitly here.
	// Deferred migrations might depend on values accumulated through migration of other actors.

	elapsed := time.Since(startTime)
	rate := float64(doneCount) / elapsed.Seconds()
	log.Log(rt.INFO, "All %d done after %v (%.0f/s). Flushing state tree root.", doneCount, elapsed, rate)
	return actorsOut.Flush()
}

type migrationInput struct {
	address.Address
	states2.Actor
	StateMigration
}
type migrationResult struct {
	address.Address
	states3.Actor
}

func migrateOneActor(ctx context.Context, store cbor.IpldStore, input *migrationInput, priorEpoch abi.ChainEpoch) (*migrationResult, error) {
	actorIn := input.Actor
	addr := input.Address
	result, err := input.MigrateState(ctx, store, StateMigrationInput{
		address:    addr,
		balance:    actorIn.Balance,
		head:       actorIn.Head,
		priorEpoch: priorEpoch,
	})
	if err != nil {
		return nil, xerrors.Errorf("state migration failed for %s actor, addr %s: %w", builtin2.ActorNameByCode(actorIn.Code), addr, err)
	}

	// Set up new actor record with the migrated state.
	return &migrationResult{
		addr, // Unchanged
		states3.Actor{
			Code:       result.NewCodeCID,
			Head:       result.NewHead,
			CallSeqNum: actorIn.CallSeqNum, // Unchanged
			Balance:    actorIn.Balance,    // Unchanged
		},
	}, nil
}
