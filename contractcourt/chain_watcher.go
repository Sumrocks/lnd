package contractcourt

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// LocalUnilateralCloseInfo encapsulates all the informnation we need to act
// on a local force close that gets confirmed.
type LocalUnilateralCloseInfo struct {
	*chainntnfs.SpendDetail
	*lnwallet.LocalForceCloseSummary
}

// ChainEventSubscription is a struct that houses a subscription to be notified
// for any on-chain events related to a channel. There are three types of
// possible on-chain events: a cooperative channel closure, a unilateral
// channel closure, and a channel breach. The fourth type: a force close is
// locally initiated, so we don't provide any event stream for said event.
type ChainEventSubscription struct {
	// ChanPoint is that channel that chain events will be dispatched for.
	ChanPoint wire.OutPoint

	// RemoteUnilateralClosure is a channel that will be sent upon in the
	// event that the remote party's commitment transaction is confirmed.
	RemoteUnilateralClosure chan *lnwallet.UnilateralCloseSummary

	// LocalUnilateralClosure is a channel that will be sent upon in the
	// event that our commitment transaction is confirmed.
	LocalUnilateralClosure chan *LocalUnilateralCloseInfo

	// CooperativeClosure is a signal that will be sent upon once a
	// cooperative channel closure has been detected confirmed.
	//
	// TODO(roasbeef): or something else
	CooperativeClosure chan struct{}

	// ContractBreach is a channel that will be sent upon if we detect a
	// contract breach. The struct sent across the channel contains all the
	// material required to bring the cheating channel peer to justice.
	ContractBreach chan *lnwallet.BreachRetribution

	// ProcessACK is a channel that will be used by the chainWatcher to
	// synchronize dispatch and processing of the notification with the act
	// of updating the state of the channel on disk. This ensures that the
	// event can be reliably handed off.
	//
	// NOTE: This channel will only be used if the syncDispatch arg passed
	// into the constructor is true.
	ProcessACK chan error

	// Cancel cancels the subscription to the event stream for a particular
	// channel. This method should be called once the caller no longer needs to
	// be notified of any on-chain events for a particular channel.
	Cancel func()
}

// chainWatcher is a system that's assigned to every active channel. The duty
// of this system is to watch the chain for spends of the channels chan point.
// If a spend is detected then with chain watcher will notify all subscribers
// that the channel has been closed, and also give them the materials necessary
// to sweep the funds of the channel on chain eventually.
type chainWatcher struct {
	started int32
	stopped int32

	quit chan struct{}
	wg   sync.WaitGroup

	// chanState is a snapshot of the persistent state of the channel that
	// we're watching. In the event of an on-chain event, we'll query the
	// database to ensure that we act using the most up to date state.
	chanState *channeldb.OpenChannel

	// stateHintObfuscator is a 48-bit state hint that's used to obfuscate
	// the current state number on the commitment transactions.
	stateHintObfuscator [lnwallet.StateHintSize]byte

	// notifier is a reference to the channel notifier that we'll use to be
	// notified of output spends and when transactions are confirmed.
	notifier chainntnfs.ChainNotifier

	// pCache is a reference to the shared preimage cache. We'll use this
	// to see if we can settle any incoming HTLC's during a remote
	// commitment close event.
	pCache WitnessBeacon

	// signer is the main signer instances that will be responsible for
	// signing any HTLC and commitment transaction generated by the state
	// machine.
	signer lnwallet.Signer

	// All the fields below are protected by this mutex.
	sync.Mutex

	// clientID is an ephemeral counter used to keep track of each
	// individual client subscription.
	clientID uint64

	// clientSubscriptions is a map that keeps track of all the active
	// client subscriptions for events related to this channel.
	clientSubscriptions map[uint64]*ChainEventSubscription

	// possibleCloses is a map from cooperative closing transaction txid to
	// a close summary that describes the nature of the channel closure.
	// We'll use this map to keep track of all possible channel closures to
	// ensure out db state is correct in the end.
	possibleCloses map[chainhash.Hash]*channeldb.ChannelCloseSummary

	// markChanClosed is a method that will be called by the watcher if it
	// detects that a cooperative closure transaction has successfully been
	// confirmed.
	markChanClosed func() error

	// isOurAddr is a function that returns true if the passed address is
	// known to us.
	isOurAddr func(btcutil.Address) bool
}

// newChainWatcher returns a new instance of a chainWatcher for a channel given
// the chan point to watch, and also a notifier instance that will allow us to
// detect on chain events.
func newChainWatcher(chanState *channeldb.OpenChannel,
	notifier chainntnfs.ChainNotifier, pCache WitnessBeacon,
	signer lnwallet.Signer, isOurAddr func(btcutil.Address) bool,
	markChanClosed func() error) (*chainWatcher, error) {

	// In order to be able to detect the nature of a potential channel
	// closure we'll need to reconstruct the state hint bytes used to
	// obfuscate the commitment state number encoded in the lock time and
	// sequence fields.
	var stateHint [lnwallet.StateHintSize]byte
	if chanState.IsInitiator {
		stateHint = lnwallet.DeriveStateHintObfuscator(
			chanState.LocalChanCfg.PaymentBasePoint.PubKey,
			chanState.RemoteChanCfg.PaymentBasePoint.PubKey,
		)
	} else {
		stateHint = lnwallet.DeriveStateHintObfuscator(
			chanState.RemoteChanCfg.PaymentBasePoint.PubKey,
			chanState.LocalChanCfg.PaymentBasePoint.PubKey,
		)
	}

	return &chainWatcher{
		chanState:           chanState,
		stateHintObfuscator: stateHint,
		notifier:            notifier,
		pCache:              pCache,
		markChanClosed:      markChanClosed,
		signer:              signer,
		quit:                make(chan struct{}),
		clientSubscriptions: make(map[uint64]*ChainEventSubscription),
		isOurAddr:           isOurAddr,
		possibleCloses:      make(map[chainhash.Hash]*channeldb.ChannelCloseSummary),
	}, nil
}

// Start starts all goroutines that the chainWatcher needs to perform its
// duties.
func (c *chainWatcher) Start() error {
	if !atomic.CompareAndSwapInt32(&c.started, 0, 1) {
		return nil
	}

	log.Debugf("Starting chain watcher for ChannelPoint(%v)",
		c.chanState.FundingOutpoint)

	// First, we'll register for a notification to be dispatched if the
	// funding output is spent.
	fundingOut := &c.chanState.FundingOutpoint

	// As a height hint, we'll try to use the opening height, but if the
	// channel isn't yet open, then we'll use the height it was broadcast
	// at.
	heightHint := c.chanState.ShortChanID.BlockHeight
	if heightHint == 0 {
		heightHint = c.chanState.FundingBroadcastHeight
	}

	spendNtfn, err := c.notifier.RegisterSpendNtfn(
		fundingOut, heightHint, false,
	)
	if err != nil {
		return err
	}

	// With the spend notification obtained, we'll now dispatch the
	// closeObserver which will properly react to any changes.
	c.wg.Add(1)
	go c.closeObserver(spendNtfn)

	return nil
}

// Stop signals the close observer to gracefully exit.
func (c *chainWatcher) Stop() error {
	if !atomic.CompareAndSwapInt32(&c.stopped, 0, 1) {
		return nil
	}

	close(c.quit)

	c.wg.Wait()

	return nil
}

// SubscribeChannelEvents returns an active subscription to the set of channel
// events for the channel watched by this chain watcher. Once clients no longer
// require the subscription, they should call the Cancel() method to allow the
// watcher to regain those committed resources. The syncDispatch bool indicates
// if the caller would like a synchronous dispatch of the notification. This
// means that the main chain watcher goroutine won't proceed with
// post-processing after the notification until the ProcessACK channel is sent
// upon.
func (c *chainWatcher) SubscribeChannelEvents(syncDispatch bool) *ChainEventSubscription {

	c.Lock()
	clientID := c.clientID
	c.clientID++
	c.Unlock()

	log.Debugf("New ChainEventSubscription(id=%v) for ChannelPoint(%v)",
		clientID, c.chanState.FundingOutpoint)

	sub := &ChainEventSubscription{
		ChanPoint:               c.chanState.FundingOutpoint,
		RemoteUnilateralClosure: make(chan *lnwallet.UnilateralCloseSummary, 1),
		LocalUnilateralClosure:  make(chan *LocalUnilateralCloseInfo, 1),
		CooperativeClosure:      make(chan struct{}, 1),
		ContractBreach:          make(chan *lnwallet.BreachRetribution, 1),
		Cancel: func() {
			c.Lock()
			delete(c.clientSubscriptions, clientID)
			c.Unlock()
			return
		},
	}

	if syncDispatch {
		sub.ProcessACK = make(chan error, 1)
	}

	c.Lock()
	c.clientSubscriptions[clientID] = sub
	c.Unlock()

	return sub
}

// closeObserver is a dedicated goroutine that will watch for any closes of the
// channel that it's watching on chain. In the event of an on-chain event, the
// close observer will assembled the proper materials required to claim the
// funds of the channel on-chain (if required), then dispatch these as
// notifications to all subscribers.
func (c *chainWatcher) closeObserver(spendNtfn *chainntnfs.SpendEvent) {
	defer c.wg.Done()

	log.Infof("Close observer for ChannelPoint(%v) active",
		c.chanState.FundingOutpoint)

	for {
		select {
		// We've detected a spend of the channel onchain! Depending on
		// the type of spend, we'll act accordingly , so we'll examine
		// the spending transaction to determine what we should do.
		//
		// TODO(Roasbeef): need to be able to ensure this only triggers
		// on confirmation, to ensure if multiple txns are broadcast, we
		// act on the one that's timestamped
		case commitSpend, ok := <-spendNtfn.Spend:
			// If the channel was closed, then this means that the
			// notifier exited, so we will as well.
			if !ok {
				return
			}

			// Otherwise, the remote party might have broadcast a
			// prior revoked state...!!!
			commitTxBroadcast := commitSpend.SpendingTx

			localCommit, remoteCommit, err := c.chanState.LatestCommitments()
			if err != nil {
				log.Errorf("Unable to fetch channel state for "+
					"chan_point=%v", c.chanState.FundingOutpoint)
				return
			}

			// We'll not retrieve the latest sate of the revocation
			// store so we can populate the information within the
			// channel state object that we have.
			//
			// TODO(roasbeef): mutation is bad mkay
			_, err = c.chanState.RemoteRevocationStore()
			if err != nil {
				log.Errorf("Unable to fetch revocation state for "+
					"chan_point=%v", c.chanState.FundingOutpoint)
				return
			}

			// If this is our commitment transaction, then we can
			// exit here as we don't have any further processing we
			// need to do (we can't cheat ourselves :p).
			commitmentHash := localCommit.CommitTx.TxHash()
			isOurCommitment := commitSpend.SpenderTxHash.IsEqual(
				&commitmentHash,
			)
			if isOurCommitment {
				if err := c.dispatchLocalForceClose(
					commitSpend, *localCommit,
				); err != nil {
					log.Errorf("unable to handle local"+
						"close for chan_point=%v: %v",
						c.chanState.FundingOutpoint, err)
				}
				return
			}

			// Next, we'll check to see if this is a cooperative
			// channel closure or not. This is characterized by
			// having an input sequence number that's finalized.
			// This won't happen with regular commitment
			// transactions due to the state hint encoding scheme.
			if commitTxBroadcast.TxIn[0].Sequence == wire.MaxTxInSequenceNum {
				err := c.dispatchCooperativeClose(commitSpend)
				if err != nil {
					log.Errorf("unable to handle co op close: %v", err)
				}
				return
			}

			log.Warnf("Unprompted commitment broadcast for "+
				"ChannelPoint(%v) ", c.chanState.FundingOutpoint)

			// Decode the state hint encoded within the commitment
			// transaction to determine if this is a revoked state
			// or not.
			obfuscator := c.stateHintObfuscator
			broadcastStateNum := lnwallet.GetStateNumHint(
				commitTxBroadcast, obfuscator,
			)
			remoteStateNum := remoteCommit.CommitHeight

			switch {
			// If state number spending transaction matches the
			// current latest state, then they've initiated a
			// unilateral close. So we'll trigger the unilateral
			// close signal so subscribers can clean up the state
			// as necessary.
			//
			// We'll also handle the case of the remote party
			// broadcasting their commitment transaction which is
			// one height above ours. This case can arise when we
			// initiate a state transition, but the remote party
			// has a fail crash _after_ accepting the new state,
			// but _before_ sending their signature to us.
			case broadcastStateNum >= remoteStateNum:
				if err := c.dispatchRemoteForceClose(
					commitSpend, *remoteCommit,
				); err != nil {
					log.Errorf("unable to handle remote "+
						"close for chan_point=%v: %v",
						c.chanState.FundingOutpoint, err)
				}

			// If the state number broadcast is lower than the
			// remote node's current un-revoked height, then
			// THEY'RE ATTEMPTING TO VIOLATE THE CONTRACT LAID OUT
			// WITHIN THE PAYMENT CHANNEL.  Therefore we close the
			// signal indicating a revoked broadcast to allow
			// subscribers to
			// swiftly dispatch justice!!!
			case broadcastStateNum < remoteStateNum:
				if err := c.dispatchContractBreach(
					commitSpend, remoteCommit,
					broadcastStateNum,
				); err != nil {
					log.Errorf("unable to handle channel "+
						"breach for chan_point=%v: %v",
						c.chanState.FundingOutpoint, err)
				}
			}

			// Now that a spend has been detected, we've done our
			// job, so we'll exit immediately.
			return

		// The chainWatcher has been signalled to exit, so we'll do so now.
		case <-c.quit:
			return
		}
	}
}

// toSelfAmount takes a transaction and returns the sum of all outputs that pay
// to a script that the wallet controls. If no outputs pay to us, then we
// return zero. This is possible as our output may have been trimmed due to
// being dust.
func (c *chainWatcher) toSelfAmount(tx *wire.MsgTx) btcutil.Amount {
	var selfAmt btcutil.Amount
	for _, txOut := range tx.TxOut {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			// Doesn't matter what net we actually pass in.
			txOut.PkScript, &chaincfg.TestNet3Params,
		)
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if c.isOurAddr(addr) {
				selfAmt += btcutil.Amount(txOut.Value)
			}
		}
	}

	return selfAmt
}

// dispatchCooperativeClose processed a detect cooperative channel closure.
// We'll use the spending transaction to locate our output within the
// transaction, then clean up the database state. We'll also dispatch a
// notification to all subscribers that the channel has been closed in this
// manner.
func (c *chainWatcher) dispatchCooperativeClose(commitSpend *chainntnfs.SpendDetail) error {
	broadcastTx := commitSpend.SpendingTx

	log.Infof("Cooperative closure for ChannelPoint(%v): %v",
		c.chanState.FundingOutpoint, spew.Sdump(broadcastTx))

	// If the input *is* final, then we'll check to see which output is
	// ours.
	localAmt := c.toSelfAmount(broadcastTx)

	// Once this is known, we'll mark the state as pending close in the
	// database.
	closeSummary := &channeldb.ChannelCloseSummary{
		ChanPoint:      c.chanState.FundingOutpoint,
		ChainHash:      c.chanState.ChainHash,
		ClosingTXID:    *commitSpend.SpenderTxHash,
		RemotePub:      c.chanState.IdentityPub,
		Capacity:       c.chanState.Capacity,
		CloseHeight:    uint32(commitSpend.SpendingHeight),
		SettledBalance: localAmt,
		CloseType:      channeldb.CooperativeClose,
		CloseStatus:    channeldb.PendingResolution,
		ShortChanID:    c.chanState.ShortChanID,
	}
	err := c.chanState.CloseChannel(closeSummary)
	if err != nil && err != channeldb.ErrNoActiveChannels &&
		err != channeldb.ErrNoChanDBExists {
		return fmt.Errorf("unable to close chan state: %v", err)
	}

	// Finally, we'll launch a goroutine to mark the channel as fully
	// closed once the transaction confirmed.
	// TODO(halseth): remove this as it should be already confirmed
	// at this point, and don't have to go via state PendingResolution.
	go func() {
		confNtfn, err := c.notifier.RegisterConfirmationsNtfn(
			commitSpend.SpenderTxHash, 1,
			uint32(commitSpend.SpendingHeight),
		)
		if err != nil {
			log.Errorf("unable to register for conf: %v", err)
			return
		}

		log.Infof("closeObserver: waiting for txid=%v to close "+
			"ChannelPoint(%v) on chain", commitSpend.SpenderTxHash,
			c.chanState.FundingOutpoint)

		select {
		case confInfo, ok := <-confNtfn.Confirmed:
			if !ok {
				log.Errorf("notifier exiting")
				return
			}

			log.Infof("closeObserver: ChannelPoint(%v) is fully "+
				"closed, at height: %v", c.chanState.FundingOutpoint,
				confInfo.BlockHeight)

			err := c.markChanClosed()
			if err != nil {
				log.Errorf("unable to mark chan fully "+
					"closed: %v", err)
				return
			}

		case <-c.quit:
			return
		}
	}()

	c.Lock()
	for _, sub := range c.clientSubscriptions {
		select {
		case sub.CooperativeClosure <- struct{}{}:
		case <-c.quit:
			c.Unlock()
			return fmt.Errorf("exiting")
		}
	}
	c.Unlock()

	return nil

}

// dispatchLocalForceClose processes a unilateral close by us being confirmed.
func (c *chainWatcher) dispatchLocalForceClose(
	commitSpend *chainntnfs.SpendDetail,
	localCommit channeldb.ChannelCommitment) error {

	log.Infof("Local unilateral close of ChannelPoint(%v) "+
		"detected", c.chanState.FundingOutpoint)

	forceClose, err := lnwallet.NewLocalForceCloseSummary(
		c.chanState, c.signer, c.pCache, commitSpend.SpendingTx,
		localCommit,
	)
	if err != nil {
		return err
	}

	// As we've detected that the channel has been closed, immediately
	// delete the state from disk, creating a close summary for future
	// usage by related sub-systems.
	chanSnapshot := forceClose.ChanSnapshot
	closeSummary := &channeldb.ChannelCloseSummary{
		ChanPoint:   chanSnapshot.ChannelPoint,
		ChainHash:   chanSnapshot.ChainHash,
		ClosingTXID: forceClose.CloseTx.TxHash(),
		RemotePub:   &chanSnapshot.RemoteIdentity,
		Capacity:    chanSnapshot.Capacity,
		CloseType:   channeldb.LocalForceClose,
		CloseStatus: channeldb.PendingResolution,
		ShortChanID: c.chanState.ShortChanID,
		CloseHeight: uint32(commitSpend.SpendingHeight),
	}

	// If our commitment output isn't dust or we have active HTLC's on the
	// commitment transaction, then we'll populate the balances on the
	// close channel summary.
	if forceClose.CommitResolution != nil {
		closeSummary.SettledBalance = chanSnapshot.LocalBalance.ToSatoshis()
		closeSummary.TimeLockedBalance = chanSnapshot.LocalBalance.ToSatoshis()
	}
	for _, htlc := range forceClose.HtlcResolutions.OutgoingHTLCs {
		htlcValue := btcutil.Amount(htlc.SweepSignDesc.Output.Value)
		closeSummary.TimeLockedBalance += htlcValue
	}
	err = c.chanState.CloseChannel(closeSummary)
	if err != nil {
		return fmt.Errorf("unable to delete channel state: %v", err)
	}

	// With the event processed, we'll now notify all subscribers of the
	// event.
	closeInfo := &LocalUnilateralCloseInfo{commitSpend, forceClose}
	c.Lock()
	for _, sub := range c.clientSubscriptions {
		select {
		case sub.LocalUnilateralClosure <- closeInfo:
		case <-c.quit:
			c.Unlock()
			return fmt.Errorf("exiting")
		}
	}
	c.Unlock()

	return nil
}

// dispatchRemoteForceClose processes a detected unilateral channel closure by the
// remote party. This function will prepare a UnilateralCloseSummary which will
// then be sent to any subscribers allowing them to resolve all our funds in
// the channel on chain. Once this close summary is prepared, all registered
// subscribers will receive a notification of this event.
func (c *chainWatcher) dispatchRemoteForceClose(commitSpend *chainntnfs.SpendDetail,
	remoteCommit channeldb.ChannelCommitment) error {

	log.Infof("Unilateral close of ChannelPoint(%v) "+
		"detected", c.chanState.FundingOutpoint)

	// First, we'll create a closure summary that contains all the
	// materials required to let each subscriber sweep the funds in the
	// channel on-chain.
	uniClose, err := lnwallet.NewUnilateralCloseSummary(c.chanState,
		c.signer, c.pCache, commitSpend, remoteCommit,
	)
	if err != nil {
		return err
	}

	// As we've detected that the channel has been closed, immediately
	// delete the state from disk, creating a close summary for future
	// usage by related sub-systems.
	err = c.chanState.CloseChannel(&uniClose.ChannelCloseSummary)
	if err != nil {
		return fmt.Errorf("unable to delete channel state: %v", err)
	}

	// With the event processed, we'll now notify all subscribers of the
	// event.
	c.Lock()
	for _, sub := range c.clientSubscriptions {
		// TODO(roasbeef): send msg before writing to disk
		//  * need to ensure proper fault tolerance in all cases
		//  * get ACK from the consumer of the ntfn before writing to disk?
		//  * no harm in repeated ntfns: at least once semantics
		select {
		case sub.RemoteUnilateralClosure <- uniClose:
		case <-c.quit:
			c.Unlock()
			return fmt.Errorf("exiting")
		}
	}
	c.Unlock()

	return nil
}

// dispatchContractBreach processes a detected contract breached by the remote
// party. This method is to be called once we detect that the remote party has
// broadcast a prior revoked commitment state. This method well prepare all the
// materials required to bring the cheater to justice, then notify all
// registered subscribers of this event.
func (c *chainWatcher) dispatchContractBreach(spendEvent *chainntnfs.SpendDetail,
	remoteCommit *channeldb.ChannelCommitment,
	broadcastStateNum uint64) error {

	log.Warnf("Remote peer has breached the channel contract for "+
		"ChannelPoint(%v). Revoked state #%v was broadcast!!!",
		c.chanState.FundingOutpoint, broadcastStateNum)

	if err := c.chanState.MarkBorked(); err != nil {
		return fmt.Errorf("unable to mark channel as borked: %v", err)
	}

	var (
		commitTxBroadcast = spendEvent.SpendingTx
		spendHeight       = uint32(spendEvent.SpendingHeight)
	)

	// Create a new reach retribution struct which contains all the data
	// needed to swiftly bring the cheating peer to justice.
	//
	// TODO(roasbeef): move to same package
	retribution, err := lnwallet.NewBreachRetribution(
		c.chanState, broadcastStateNum, commitTxBroadcast,
		spendHeight,
	)
	if err != nil {
		return fmt.Errorf("unable to create breach retribution: %v", err)
	}

	// Nil the curve before printing.
	if retribution.RemoteOutputSignDesc != nil &&
		retribution.RemoteOutputSignDesc.DoubleTweak != nil {
		retribution.RemoteOutputSignDesc.DoubleTweak.Curve = nil
	}
	if retribution.LocalOutputSignDesc != nil &&
		retribution.LocalOutputSignDesc.DoubleTweak != nil {
		retribution.LocalOutputSignDesc.DoubleTweak.Curve = nil
	}

	log.Debugf("Punishment breach retribution created: %v",
		newLogClosure(func() string {
			return spew.Sdump(retribution)
		}))

	// With the event processed, we'll now notify all subscribers of the
	// event.
	c.Lock()
	for _, sub := range c.clientSubscriptions {
		select {
		case sub.ContractBreach <- retribution:
		case <-c.quit:
			c.Unlock()
			return fmt.Errorf("quitting")
		}

		// Wait for the breach arbiter to ACK the handoff before
		// marking the channel as pending force closed in channeldb,
		// but only if the client requested a sync dispatch.
		if sub.ProcessACK != nil {
			select {
			case err := <-sub.ProcessACK:
				// Bail if the handoff failed.
				if err != nil {
					c.Unlock()
					return fmt.Errorf("unable to handoff "+
						"retribution info: %v", err)
				}

			case <-c.quit:
				c.Unlock()
				return fmt.Errorf("quitting")
			}
		}
	}
	c.Unlock()

	// At this point, we've successfully received an ack for the breach
	// close. We now construct and persist  the close summary, marking the
	// channel as pending force closed.
	//
	// TODO(roasbeef): instead mark we got all the monies?
	settledBalance := remoteCommit.LocalBalance.ToSatoshis()
	closeSummary := channeldb.ChannelCloseSummary{
		ChanPoint:      c.chanState.FundingOutpoint,
		ChainHash:      c.chanState.ChainHash,
		ClosingTXID:    *spendEvent.SpenderTxHash,
		CloseHeight:    spendHeight,
		RemotePub:      c.chanState.IdentityPub,
		Capacity:       c.chanState.Capacity,
		SettledBalance: settledBalance,
		CloseType:      channeldb.BreachClose,
		CloseStatus:    channeldb.PendingResolution,
		ShortChanID:    c.chanState.ShortChanID,
	}

	log.Infof("Breached channel=%v marked pending-closed",
		c.chanState.FundingOutpoint)

	return c.chanState.CloseChannel(&closeSummary)
}

// CooperativeCloseCtx is a transactional object that's used by external
// parties to initiate a cooperative closure negotiation. During the
// negotiation, we sign multiple versions of a closing transaction, either of
// which may be counter signed and broadcast by the remote party at any time.
// As a result, we'll need to watch the chain to see if any of these confirm,
// only afterwards will we mark the channel as fully closed.
type CooperativeCloseCtx struct {
	// potentialCloses is a channel will be used by the party negotiating
	// the cooperative closure to send possible closing states to the chain
	// watcher to ensure we detect all on-chain spends.
	potentialCloses chan *channeldb.ChannelCloseSummary

	// activeCloses keeps track of all the txid's that we're currently
	// watching for.
	activeCloses map[chainhash.Hash]struct{}

	// watchCancel will be closed once *one* of the txid's in the map above
	// is confirmed. This will cause all the lingering goroutines to exit.
	watchCancel chan struct{}

	watcher *chainWatcher

	sync.Mutex
}

// BeginCooperativeClose should be called by the party negotiating the
// cooperative closure before the first signature is sent to the remote party.
// This will return a context that should be used to communicate possible
// closing states so we can act on them.
func (c *chainWatcher) BeginCooperativeClose() *CooperativeCloseCtx {
	// We'll simply return a new close context that will be used be the
	// caller to notify us of potential closes.
	return &CooperativeCloseCtx{
		potentialCloses: make(chan *channeldb.ChannelCloseSummary),
		watchCancel:     make(chan struct{}),
		activeCloses:    make(map[chainhash.Hash]struct{}),
		watcher:         c,
	}
}

// LogPotentialClose should be called by the party negotiating the cooperative
// closure once they signed a new state, but *before* they transmit it to the
// remote party. This will ensure that the chain watcher is able to log the new
// state it should watch the chain for.
func (c *CooperativeCloseCtx) LogPotentialClose(potentialClose *channeldb.ChannelCloseSummary) {
	c.Lock()
	defer c.Unlock()

	// We'll check to see if we're already watching for a close of this
	// channel, if so, then we'll exit early to avoid launching a duplicate
	// goroutine.
	if _, ok := c.activeCloses[potentialClose.ClosingTXID]; ok {
		return
	}

	// Otherwise, we'll mark this txid as currently being watched.
	c.activeCloses[potentialClose.ClosingTXID] = struct{}{}

	// We'll take this potential close, and launch a goroutine which will
	// wait until it's confirmed, then update the database state. When a
	// potential close gets confirmed, we'll cancel out all other launched
	// goroutines.
	go func() {
		confNtfn, err := c.watcher.notifier.RegisterConfirmationsNtfn(
			&potentialClose.ClosingTXID, 1,
			uint32(potentialClose.CloseHeight),
		)
		if err != nil {
			log.Errorf("unable to register for conf: %v", err)
			return
		}

		log.Infof("closeCtx: waiting for txid=%v to close "+
			"ChannelPoint(%v) on chain", potentialClose.ClosingTXID,
			c.watcher.chanState.FundingOutpoint)

		select {
		case confInfo, ok := <-confNtfn.Confirmed:
			if !ok {
				log.Errorf("notifier exiting")
				return
			}

			log.Infof("closeCtx: ChannelPoint(%v) is fully closed, at "+
				"height: %v", c.watcher.chanState.FundingOutpoint,
				confInfo.BlockHeight)

			close(c.watchCancel)

			c.watcher.Lock()
			for _, sub := range c.watcher.clientSubscriptions {
				select {
				case sub.CooperativeClosure <- struct{}{}:
				case <-c.watcher.quit:
				}
			}
			c.watcher.Unlock()

			err := c.watcher.chanState.CloseChannel(potentialClose)
			if err != nil {
				log.Warnf("closeCtx: unable to update latest "+
					"close for ChannelPoint(%v): %v",
					c.watcher.chanState.FundingOutpoint, err)
			}

			err = c.watcher.markChanClosed()
			if err != nil {
				log.Errorf("closeCtx: unable to mark chan fully "+
					"closed: %v", err)
				return
			}

		case <-c.watchCancel:
			log.Debugf("Exiting watch for close of txid=%v for "+
				"ChannelPoint(%v)", potentialClose.ClosingTXID,
				c.watcher.chanState.FundingOutpoint)

		case <-c.watcher.quit:
			return
		}
	}()
}

// Finalize should be called once both parties agree on a final transaction to
// close out the channel. This method will immediately mark the channel as
// pending closed in the database, then launch a goroutine to mark the channel
// fully closed upon confirmation.
func (c *CooperativeCloseCtx) Finalize(preferredClose *channeldb.ChannelCloseSummary) error {
	chanPoint := c.watcher.chanState.FundingOutpoint

	log.Infof("Finalizing chan close for ChannelPoint(%v)", chanPoint)

	err := c.watcher.chanState.CloseChannel(preferredClose)
	if err != nil {
		log.Errorf("closeCtx: unable to close ChannelPoint(%v): %v",
			chanPoint, err)
		return err
	}

	go c.LogPotentialClose(preferredClose)

	return nil
}
