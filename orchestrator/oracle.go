package orchestrator

import (
	"context"
	"time"

	"github.com/avast/retry-go"
	"github.com/pkg/errors"
	log "github.com/xlab/suplog"

	"github.com/InjectiveLabs/metrics"
	"github.com/InjectiveLabs/peggo/orchestrator/loops"
	wrappers "github.com/InjectiveLabs/peggo/solidity/wrappers/Peggy.sol"
)

// Considering blocktime of up to 3 seconds approx on the Injective Chain and an oracle loop duration = 1 minute,
// we broadcast only 20 events in each iteration.
// So better to search only 20 blocks to ensure all the events are broadcast to Injective Chain without misses.
const (
	ethBlockConfirmationDelay uint64 = 12
	defaultBlocksToSearch     uint64 = 20
)

// EthOracleMainLoop is responsible for making sure that Ethereum events are retrieved from the Ethereum blockchain
// and ferried over to Cosmos where they will be used to issue tokens or process batches.
func (s *PeggyOrchestrator) EthOracleMainLoop(ctx context.Context) error {
	logger := log.WithField("loop", "EthOracleMainLoop")
	lastResync := time.Now()

	var lastConfirmedEthHeight uint64
	retryFn := func() error {
		height, err := s.getLastConfirmedEthHeight(ctx)
		if err != nil {
			logger.WithError(err).Warningf("failed to get last claim from Injective. Querying peggy params...")
		}

		if height == 0 {
			peggyParams, err := s.injective.PeggyParams(ctx)
			if err != nil {
				logger.WithError(err).Fatalln("failed to query peggy params, is injectived running?")
			}
			height = peggyParams.BridgeContractStartHeight
		}
		lastConfirmedEthHeight = height
		return nil
	}

	if err := retry.Do(retryFn,
		retry.Context(ctx),
		retry.Attempts(s.maxAttempts),
		retry.OnRetry(func(n uint, err error) {
			logger.WithError(err).Warningf("failed to get last confirmed Ethereum height from Injective, will retry (%d)", n)
		}),
	); err != nil {
		logger.WithError(err).Errorln("got error, loop exits")
		return err
	}

	return loops.RunLoop(ctx, defaultLoopDur, func() error {
		logger.WithField("last_confirmed_eth_height", lastConfirmedEthHeight).Infoln("scanning for events")

		// Relays events from Ethereum -> Cosmos
		var currentHeight uint64
		if err := retry.Do(func() (err error) {
			currentHeight, err = s.relayEthEvents(ctx, lastConfirmedEthHeight)
			return
		},
			retry.Context(ctx),
			retry.Attempts(s.maxAttempts),
			retry.OnRetry(func(n uint, err error) {
				logger.WithError(err).Warningf("error during Eth event checking, will retry (%d)", n)
			}),
		); err != nil {
			logger.WithError(err).Errorln("got error, loop exits")
			return err
		}

		lastConfirmedEthHeight = currentHeight

		/**
			Auto re-sync to catch up the nonce. Reasons why event nonce fall behind.
				1. It takes some time for events to be indexed on Ethereum. So if peggo queried events immediately as block produced, there is a chance the event is missed.
				   we need to re-scan this block to ensure events are not missed due to indexing delay.
				2. if validator was in UnBonding state, the claims broadcasted in last iteration are failed.
				3. if infura call failed while filtering events, the peggo missed to broadcast claim events occured in last iteration.
		**/

		if time.Since(lastResync) >= 48*time.Hour {
			if err := retry.Do(func() (err error) {
				lastConfirmedEthHeight, err = s.getLastConfirmedEthHeight(ctx)
				return
			},
				retry.Context(ctx),
				retry.Attempts(s.maxAttempts),
				retry.OnRetry(func(n uint, err error) {
					logger.WithError(err).Warningf("failed to get last confirmed eth height, will retry (%d)", n)
				}),
			); err != nil {
				logger.WithError(err).Errorln("got error, loop exits")
				return err
			}

			lastResync = time.Now()
			logger.WithFields(log.Fields{"last_resync": lastResync, "last_confirmed_eth_height": lastConfirmedEthHeight}).Infoln("auto resync")
		}

		return nil
	})
}

// getLastConfirmedEthHeight retrieves the last claim event this oracle has relayed to Cosmos.
func (s *PeggyOrchestrator) getLastConfirmedEthHeight(ctx context.Context) (uint64, error) {
	metrics.ReportFuncCall(s.svcTags)
	doneFn := metrics.ReportFuncTiming(s.svcTags)
	defer doneFn()

	lastClaimEvent, err := s.injective.LastClaimEvent(ctx)
	if err != nil {
		metrics.ReportFuncError(s.svcTags)
		return uint64(0), err
	}

	return lastClaimEvent.EthereumEventHeight, nil
}

// relayEthEvents checks for events such as a deposit to the Peggy Ethereum contract or a validator set update
// or a transaction batch update. It then responds to these events by performing actions on the Cosmos chain if required
func (s *PeggyOrchestrator) relayEthEvents(
	ctx context.Context,
	startingBlock uint64,
) (uint64, error) {
	metrics.ReportFuncCall(s.svcTags)
	doneFn := metrics.ReportFuncTiming(s.svcTags)
	defer doneFn()

	latestHeader, err := s.ethereum.HeaderByNumber(ctx, nil)
	if err != nil {
		metrics.ReportFuncError(s.svcTags)
		return 0, errors.Wrap(err, "failed to get latest ethereum header")
	}

	// add delay to ensure minimum confirmations are received and block is finalised
	currentBlock := latestHeader.Number.Uint64() - ethBlockConfirmationDelay
	if currentBlock < startingBlock {
		return currentBlock, nil
	}

	if currentBlock > startingBlock+defaultBlocksToSearch {
		currentBlock = startingBlock + defaultBlocksToSearch
	}

	legacyDeposits, err := s.ethereum.GetSendToCosmosEvents(startingBlock, currentBlock)
	if err != nil {
		metrics.ReportFuncError(s.svcTags)
		return 0, errors.Wrap(err, "failed to get SendToCosmos events")
	}

	deposits, err := s.ethereum.GetSendToInjectiveEvents(startingBlock, currentBlock)
	if err != nil {
		metrics.ReportFuncError(s.svcTags)
		return 0, errors.Wrap(err, "failed to get SendToInjective events")
	}

	withdrawals, err := s.ethereum.GetTransactionBatchExecutedEvents(startingBlock, currentBlock)
	if err != nil {
		metrics.ReportFuncError(s.svcTags)
		return 0, errors.Wrap(err, "failed to get TransactionBatchExecuted events")
	}

	erc20Deployments, err := s.ethereum.GetPeggyERC20DeployedEvents(startingBlock, currentBlock)
	if err != nil {
		metrics.ReportFuncError(s.svcTags)
		return 0, errors.Wrap(err, "failed to get ERC20Deployed events")
	}

	valsetUpdates, err := s.ethereum.GetValsetUpdatedEvents(startingBlock, currentBlock)
	if err != nil {
		metrics.ReportFuncError(s.svcTags)
		return 0, errors.Wrap(err, "failed to get ValsetUpdated events")
	}

	// note that starting block overlaps with our last checked block, because we have to deal with
	// the possibility that the relayer was killed after relaying only one of multiple events in a single
	// block, so we also need this routine so make sure we don't send in the first event in this hypothetical
	// multi event block again. In theory we only send all events for every block and that will pass of fail
	// atomically but lets not take that risk.
	lastClaimEvent, err := s.injective.LastClaimEvent(ctx)
	if err != nil {
		metrics.ReportFuncError(s.svcTags)
		return 0, errors.New("failed to query last claim event from Injective")
	}

	legacyDeposits = filterSendToCosmosEventsByNonce(legacyDeposits, lastClaimEvent.EthereumEventNonce)

	log.WithFields(log.Fields{
		"start":        startingBlock,
		"end":          currentBlock,
		"old_deposits": legacyDeposits,
	}).Debugln("scanned SendToCosmos events from Ethereum")

	deposits = filterSendToInjectiveEventsByNonce(deposits, lastClaimEvent.EthereumEventNonce)

	log.WithFields(log.Fields{
		"start":    startingBlock,
		"end":      currentBlock,
		"deposits": deposits,
	}).Debugln("scanned SendToInjective events from Ethereum")

	withdrawals = filterTransactionBatchExecutedEventsByNonce(withdrawals, lastClaimEvent.EthereumEventNonce)

	log.WithFields(log.Fields{
		"start":       startingBlock,
		"end":         currentBlock,
		"withdrawals": withdrawals,
	}).Debugln("scanned TransactionBatchExecuted events from Ethereum")

	erc20Deployments = filterERC20DeployedEventsByNonce(erc20Deployments, lastClaimEvent.EthereumEventNonce)

	log.WithFields(log.Fields{
		"start":             startingBlock,
		"end":               currentBlock,
		"erc20_deployments": erc20Deployments,
	}).Debugln("scanned FilterERC20Deployed events from Ethereum")

	valsetUpdates = filterValsetUpdateEventsByNonce(valsetUpdates, lastClaimEvent.EthereumEventNonce)

	log.WithFields(log.Fields{
		"start":          startingBlock,
		"end":            currentBlock,
		"valset_updates": valsetUpdates,
	}).Debugln("scanned ValsetUpdated events from Ethereum")

	if len(legacyDeposits) > 0 || len(deposits) > 0 || len(withdrawals) > 0 || len(erc20Deployments) > 0 || len(valsetUpdates) > 0 {
		// todo get eth chain id from the chain
		if err := s.injective.SendEthereumClaims(ctx,
			lastClaimEvent.EthereumEventNonce,
			legacyDeposits,
			deposits,
			withdrawals,
			erc20Deployments,
			valsetUpdates,
		); err != nil {
			metrics.ReportFuncError(s.svcTags)
			return 0, errors.Wrap(err, "failed to send event claims to Injective")
		}
	}

	return currentBlock, nil
}

func filterSendToCosmosEventsByNonce(
	events []*wrappers.PeggySendToCosmosEvent,
	nonce uint64,
) []*wrappers.PeggySendToCosmosEvent {
	res := make([]*wrappers.PeggySendToCosmosEvent, 0, len(events))

	for _, ev := range events {
		if ev.EventNonce.Uint64() > nonce {
			res = append(res, ev)
		}
	}

	return res
}

func filterSendToInjectiveEventsByNonce(
	events []*wrappers.PeggySendToInjectiveEvent,
	nonce uint64,
) []*wrappers.PeggySendToInjectiveEvent {
	res := make([]*wrappers.PeggySendToInjectiveEvent, 0, len(events))

	for _, ev := range events {
		if ev.EventNonce.Uint64() > nonce {
			res = append(res, ev)
		}
	}

	return res
}

func filterTransactionBatchExecutedEventsByNonce(
	events []*wrappers.PeggyTransactionBatchExecutedEvent,
	nonce uint64,
) []*wrappers.PeggyTransactionBatchExecutedEvent {
	res := make([]*wrappers.PeggyTransactionBatchExecutedEvent, 0, len(events))

	for _, ev := range events {
		if ev.EventNonce.Uint64() > nonce {
			res = append(res, ev)
		}
	}

	return res
}

func filterERC20DeployedEventsByNonce(
	events []*wrappers.PeggyERC20DeployedEvent,
	nonce uint64,
) []*wrappers.PeggyERC20DeployedEvent {
	res := make([]*wrappers.PeggyERC20DeployedEvent, 0, len(events))

	for _, ev := range events {
		if ev.EventNonce.Uint64() > nonce {
			res = append(res, ev)
		}
	}

	return res
}

func filterValsetUpdateEventsByNonce(
	events []*wrappers.PeggyValsetUpdatedEvent,
	nonce uint64,
) []*wrappers.PeggyValsetUpdatedEvent {
	res := make([]*wrappers.PeggyValsetUpdatedEvent, 0, len(events))

	for _, ev := range events {
		if ev.EventNonce.Uint64() > nonce {
			res = append(res, ev)
		}
	}
	return res
}
