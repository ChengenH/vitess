/*
Copyright 2021 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package reparentutil

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"k8s.io/apimachinery/pkg/util/sets"

	"vitess.io/vitess/go/event"
	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/stats"
	"vitess.io/vitess/go/vt/concurrency"
	"vitess.io/vitess/go/vt/logutil"
	logutilpb "vitess.io/vitess/go/vt/proto/logutil"
	replicationdatapb "vitess.io/vitess/go/vt/proto/replicationdata"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/topoproto"
	"vitess.io/vitess/go/vt/topotools/events"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vttablet/tmclient"
)

// EmergencyReparenter performs EmergencyReparentShard operations.
type EmergencyReparenter struct {
	ts     *topo.Server
	tmc    tmclient.TabletManagerClient
	logger logutil.Logger
}

// EmergencyReparentOptions provides optional parameters to
// EmergencyReparentShard operations. Options are passed by value, so it is safe
// for callers to mutate and reuse options structs for multiple calls.
type EmergencyReparentOptions struct {
	NewPrimaryAlias           *topodatapb.TabletAlias
	IgnoreReplicas            sets.String
	WaitReplicasTimeout       time.Duration
	PreventCrossCellPromotion bool

	// Private options managed internally. We use value passing to avoid leaking
	// these details back out.
	lockAction string
}

// counters for Emergency Reparent Shard
var (
	ersCounter        = stats.NewGauge("ers_counter", "Number of times Emergency Reparent Shard has been run")
	ersSuccessCounter = stats.NewGauge("ers_success_counter", "Number of times Emergency Reparent Shard has succeeded")
	ersFailureCounter = stats.NewGauge("ers_failure_counter", "Number of times Emergency Reparent Shard has failed")
)

// NewEmergencyReparenter returns a new EmergencyReparenter object, ready to
// perform EmergencyReparentShard operations using the given topo.Server,
// TabletManagerClient, and logger.
//
// Providing a nil logger instance is allowed.
func NewEmergencyReparenter(ts *topo.Server, tmc tmclient.TabletManagerClient, logger logutil.Logger) *EmergencyReparenter {
	erp := EmergencyReparenter{
		ts:     ts,
		tmc:    tmc,
		logger: logger,
	}

	if erp.logger == nil {
		// Create a no-op logger so we can call functions on er.logger without
		// needed to constantly check for non-nil.
		erp.logger = logutil.NewCallbackLogger(func(*logutilpb.Event) {})
	}

	return &erp
}

// ReparentShard performs the EmergencyReparentShard operation on the given
// keyspace and shard.
func (erp *EmergencyReparenter) ReparentShard(ctx context.Context, keyspace string, shard string, opts EmergencyReparentOptions) (*events.Reparent, error) {
	// First step is to lock the shard for the given operation
	opts.lockAction = erp.getLockAction(opts.NewPrimaryAlias)
	ctx, unlock, err := erp.ts.LockShard(ctx, keyspace, shard, opts.lockAction)
	if err != nil {
		return nil, err
	}
	// defer the unlock-shard function
	defer unlock(&err)

	// dispatch success or failure of ERS
	ev := &events.Reparent{}
	defer func() {
		switch err {
		case nil:
			ersSuccessCounter.Add(1)
			event.DispatchUpdate(ev, "finished EmergencyReparentShard")
		default:
			ersFailureCounter.Add(1)
			event.DispatchUpdate(ev, "failed EmergencyReparentShard: "+err.Error())
		}
	}()

	// run ERS with shard already locked
	err = erp.reparentShardLocked(ctx, ev, keyspace, shard, opts)

	return ev, err
}

func (erp *EmergencyReparenter) getLockAction(newPrimaryAlias *topodatapb.TabletAlias) string {
	action := "EmergencyReparentShard"

	if newPrimaryAlias != nil {
		action += fmt.Sprintf("(%v)", topoproto.TabletAliasString(newPrimaryAlias))
	}

	return action
}

// reparentShardLocked performs Emergency Reparent Shard operation assuming that the shard is already locked
func (erp *EmergencyReparenter) reparentShardLocked(ctx context.Context, ev *events.Reparent, keyspace, shard string, opts EmergencyReparentOptions) (err error) {
	// log the starting of the operation and increment the counter
	erp.logger.Infof("will initiate emergency reparent shard in keyspace - %s, shard - %s", keyspace, shard)
	ersCounter.Add(1)

	// get the shard information from the topology server
	var shardInfo *topo.ShardInfo
	shardInfo, err = erp.ts.GetShard(ctx, keyspace, shard)
	if err != nil {
		return err
	}
	ev.ShardInfo = *shardInfo

	// get the previous primary according to the topology server,
	// we use this information to choose the best candidate in the same cell
	// and to undo promotion in case of failure
	var prevPrimary *topodatapb.Tablet
	if shardInfo.PrimaryAlias != nil {
		prevPrimaryInfo, err := erp.ts.GetTablet(ctx, shardInfo.PrimaryAlias)
		if err != nil {
			return err
		}
		prevPrimary = prevPrimaryInfo.Tablet
	}

	// read all the tablets and there information
	event.DispatchUpdate(ev, "reading all tablets")
	var tabletMap map[string]*topo.TabletInfo
	tabletMap, err = erp.ts.GetTabletMapForShard(ctx, keyspace, shard)
	if err != nil {
		return vterrors.Wrapf(err, "failed to get tablet map for %v/%v: %v", keyspace, shard, err)
	}

	// Stop replication on all the tablets and build their status map
	var statusMap map[string]*replicationdatapb.StopReplicationStatus
	var primaryStatusMap map[string]*replicationdatapb.PrimaryStatus
	statusMap, primaryStatusMap, err = StopReplicationAndBuildStatusMaps(ctx, erp.tmc, ev, tabletMap, opts.WaitReplicasTimeout, opts.IgnoreReplicas, erp.logger)
	if err != nil {
		return vterrors.Wrapf(err, "failed to stop replication and build status maps: %v", err)
	}

	// check that we still have the shard lock. If we don't then we can terminate at this point
	if err := topo.CheckShardLocked(ctx, keyspace, shard); err != nil {
		return vterrors.Wrapf(err, "lost topology lock, aborting: %v", err)
	}

	// find the valid candidates for becoming the primary
	// this is where we check for errant GTIDs and remove the tablets that have them from consideration
	var validCandidates map[string]mysql.Position
	validCandidates, err = FindValidEmergencyReparentCandidates(statusMap, primaryStatusMap)
	if err != nil {
		return err
	}
	// Now, we restrict the valid candidates list, which removes some tablets from consideration
	validCandidates, err = restrictValidCandidates(validCandidates, tabletMap)
	if err != nil {
		return err
	} else if len(validCandidates) == 0 {
		return vterrors.Errorf(vtrpc.Code_FAILED_PRECONDITION, "no valid candidates for emergency reparent")
	}

	// Wait for all candidates to apply relay logs
	if err = erp.waitForAllRelayLogsToApply(ctx, validCandidates, tabletMap, statusMap, opts.WaitReplicasTimeout); err != nil {
		return err
	}

	// find the intermediate replication source that we want to replicate from. This will always be the most advanced tablet that we have
	// We let all the other tablets replicate from this tablet. We will then try to choose a better candidate and let it catch up
	var intermediateSource *topodatapb.Tablet
	var validCandidateTablets []*topodatapb.Tablet
	intermediateSource, validCandidateTablets, err = erp.findMostAdvanced(prevPrimary, validCandidates, tabletMap, opts)
	if err != nil {
		return err
	}
	erp.logger.Infof("intermediate primary selected - %v", intermediateSource.Alias)

	// check weather the primary candidate selected is ideal or if it can be improved later
	var isIdeal bool
	isIdeal, err = erp.intermediateCandidateIsIdeal(intermediateSource, prevPrimary, validCandidateTablets, tabletMap, opts)
	if err != nil {
		return err
	}
	erp.logger.Infof("intermediate primary is ideal - %v", isIdeal)

	// Check (again) we still have the topology lock.
	if err = topo.CheckShardLocked(ctx, keyspace, shard); err != nil {
		return vterrors.Wrapf(err, "lost topology lock, aborting: %v", err)
	}

	// initialize the newPrimary with the intermediate primary, override this value if it is not the ideal candidate
	newPrimary := intermediateSource
	if !isIdeal {
		// we now promote our intermediate primary candidate and also reparent all the other tablets to start replicating from this candidate
		// we do not promote the tablet or change the shard record. We only change the replication for all the other tablets
		// it also returns the list of the tablets that started replication successfully including itself. These are the candidates that we can use to find a replacement
		var validReplacementCandidates []*topodatapb.Tablet
		validReplacementCandidates, err = erp.promoteIntermediatePrimary(ctx, ev, intermediateSource, tabletMap, statusMap, opts)
		if err != nil {
			return err
		}

		// try to find a better candidate using the list we got back
		var betterCandidate *topodatapb.Tablet
		betterCandidate, err = erp.identifyPrimaryCandidate(intermediateSource, prevPrimary, validReplacementCandidates, tabletMap, opts)
		if err != nil {
			return err
		}

		// if our better candidate is different from our previous candidate, then we wait for it to catch up to the intermediate primary
		if !topoproto.TabletAliasEqual(betterCandidate.Alias, intermediateSource.Alias) {
			err = waitForCatchUp(ctx, erp.tmc, erp.logger, intermediateSource, betterCandidate, opts.WaitReplicasTimeout)
			if err != nil {
				return err
			}
			newPrimary = betterCandidate
		}
	}

	// now we check if all the constraints are satisfied. If they are not, then we should abort
	constraintFailure := erp.checkIfConstraintsSatisfied(newPrimary, prevPrimary, opts)
	if constraintFailure != nil {
		erp.logger.Errorf("have to override promotion because of constraint failure - %v", constraintFailure)
		// we want to send both the errors to the user, constraint failure and also any error encountered in undoing the promotion
		defer func() {
			if err != nil {
				err = vterrors.Errorf(vtrpc.Code_ABORTED, "error in undoing promotion - %v, constraint failure - %v", err, constraintFailure)
			} else {
				err = constraintFailure
			}
		}()
		// we now try to undo are changes. We can do so by promoting the previous primary instead of the new one we selected
		if prevPrimary == nil {
			return vterrors.Errorf(vtrpc.Code_ABORTED, "could not undo promotion, since shard record has no primary information")
		}
		newPrimary = prevPrimary
	}

	// Final step is to promote our primary candidate
	err = erp.promoteNewPrimary(ctx, ev, newPrimary, opts, tabletMap, statusMap)
	if err != nil {
		return err
	}

	ev.NewPrimary = proto.Clone(newPrimary).(*topodatapb.Tablet)
	return err
}

func (erp *EmergencyReparenter) waitForAllRelayLogsToApply(ctx context.Context, validCandidates map[string]mysql.Position, tabletMap map[string]*topo.TabletInfo, statusMap map[string]*replicationdatapb.StopReplicationStatus, waitReplicasTimeout time.Duration) error {
	errCh := make(chan error)
	defer close(errCh)

	groupCtx, groupCancel := context.WithTimeout(ctx, waitReplicasTimeout)
	defer groupCancel()

	waiterCount := 0

	for candidate := range validCandidates {
		// When we called StopReplicationAndBuildStatusMaps, we got back two
		// maps: (1) the StopReplicationStatus of any replicas that actually
		// stopped replication; and (2) the MasterStatus of anything that
		// returned ErrNotReplica, which is a tablet that is either the current
		// primary or is stuck thinking it is a MASTER but is not in actuality.
		//
		// If we have a tablet in the validCandidates map that does not appear
		// in the statusMap, then we have either (a) the current primary, which
		// is not replicating, so it is not applying relay logs; or (b) a tablet
		// that is stuck thinking it is MASTER but is not in actuality. In that
		// second case - (b) - we will most likely find that the stuck MASTER
		// does not have a winning position, and fail the ERS. If, on the other
		// hand, it does have a winning position, we are trusting the operator
		// to know what they are doing by emergency-reparenting onto that
		// tablet. In either case, it does not make sense to wait for relay logs
		// to apply on a tablet that was never applying relay logs in the first
		// place, so we skip it, and log that we did.
		status, ok := statusMap[candidate]
		if !ok {
			erp.logger.Infof("EmergencyReparent candidate %v not in replica status map; this means it was not running replication (because it was formerly PRIMARY), so skipping WaitForRelayLogsToApply step for this candidate", candidate)
			continue
		}

		go func(alias string, status *replicationdatapb.StopReplicationStatus) {
			var err error
			defer func() { errCh <- err }()
			err = WaitForRelayLogsToApply(groupCtx, erp.tmc, tabletMap[alias], status)
		}(candidate, status)

		waiterCount++
	}

	errgroup := concurrency.ErrorGroup{
		NumGoroutines:        waiterCount,
		NumRequiredSuccesses: waiterCount,
		NumAllowedErrors:     0,
	}
	rec := errgroup.Wait(groupCancel, errCh)

	if len(rec.Errors) != 0 {
		return vterrors.Wrapf(rec.Error(), "could not apply all relay logs within the provided waitReplicasTimeout (%s): %v", waitReplicasTimeout, rec.Error())
	}

	return nil
}

// findMostAdvanced finds the intermediate primary candidate for ERS. We always choose the most advanced one from our valid candidates list
func (erp *EmergencyReparenter) findMostAdvanced(prevPrimary *topodatapb.Tablet, validCandidates map[string]mysql.Position, tabletMap map[string]*topo.TabletInfo, opts EmergencyReparentOptions) (*topodatapb.Tablet, []*topodatapb.Tablet, error) {
	erp.logger.Infof("started finding the intermediate primary candidate")
	// convert the valid candidates into a list so that we can use it for sorting
	validTablets, tabletPositions, err := getValidCandidatesAndPositionsAsList(validCandidates, tabletMap)
	if err != nil {
		return nil, nil, err
	}

	idealCell := ""
	if prevPrimary != nil {
		idealCell = prevPrimary.Alias.Cell
	}

	// sort the tablets for finding the best intermediate primary in ERS
	err = sortTabletsForERS(validTablets, tabletPositions, idealCell)
	if err != nil {
		return nil, nil, err
	}
	for _, tablet := range validTablets {
		erp.logger.Infof("finding intermediate primary - sorted replica: %v", tablet.Alias)
	}

	// The first tablet in the sorted list will be the most eligible candidate unless explicitly asked for some other tablet
	winningPrimaryTablet := validTablets[0]
	winningPosition := tabletPositions[0]

	// We have already removed the tablets with errant GTIDs before calling this function. At this point our winning position must be a
	// superset of all the other valid positions. If that is not the case, then we have a split brain scenario, and we should cancel the ERS
	for i, position := range tabletPositions {
		if !winningPosition.AtLeast(position) {
			return nil, nil, vterrors.Errorf(vtrpc.Code_FAILED_PRECONDITION, "split brain detected between servers - %v and %v", winningPrimaryTablet.Alias, validTablets[i].Alias)
		}
	}

	// If we were requested to elect a particular primary, verify it's a valid
	// candidate (non-zero position, no errant GTIDs)
	// Also, if the candidate is
	if opts.NewPrimaryAlias != nil {
		requestedPrimaryAlias := topoproto.TabletAliasString(opts.NewPrimaryAlias)
		pos, ok := validCandidates[requestedPrimaryAlias]
		if !ok {
			return nil, nil, vterrors.Errorf(vtrpc.Code_FAILED_PRECONDITION, "requested primary elect %v has errant GTIDs", requestedPrimaryAlias)
		}
		// if the requested tablet is as advanced as the most advanced tablet, then we can just use it for promotion.
		// otherwise, we should let it catchup to the most advanced tablet and let it be the intermediate primary
		if pos.AtLeast(winningPosition) {
			requestedPrimaryInfo, isFound := tabletMap[requestedPrimaryAlias]
			if !isFound {
				return nil, nil, vterrors.Errorf(vtrpc.Code_INTERNAL, "candidate %v not found in the tablet map; this an impossible situation", requestedPrimaryAlias)
			}
			winningPrimaryTablet = requestedPrimaryInfo.Tablet
		}
	}

	return winningPrimaryTablet, validTablets, nil
}

// promoteIntermediatePrimary promotes the primary candidate that we have, but it does not yet set to start accepting writes
func (erp *EmergencyReparenter) promoteIntermediatePrimary(ctx context.Context, ev *events.Reparent, newPrimary *topodatapb.Tablet, tabletMap map[string]*topo.TabletInfo, statusMap map[string]*replicationdatapb.StopReplicationStatus, opts EmergencyReparentOptions) ([]*topodatapb.Tablet, error) {
	// we reparent all the other tablets to start replication from our new primary
	// we wait for all the replicas so that we can choose a better candidate from the ones that started replication later
	validCandidatesForImprovement, err := erp.reparentReplicas(ctx, ev, newPrimary, tabletMap, statusMap, opts, true, false)
	if err != nil {
		return nil, err
	}

	// also include the current tablet for being considered as part of valid candidates for ERS promotion
	validCandidatesForImprovement = append(validCandidatesForImprovement, newPrimary)
	return validCandidatesForImprovement, nil
}

// reparentReplicas reparents all the replicas provided and populates the reparent journal on the primary.
// Also, it returns the replicas which started replicating only in the case where we wait for all the replicas
func (erp *EmergencyReparenter) reparentReplicas(ctx context.Context, ev *events.Reparent, newPrimaryTablet *topodatapb.Tablet, tabletMap map[string]*topo.TabletInfo, statusMap map[string]*replicationdatapb.StopReplicationStatus, opts EmergencyReparentOptions, waitForAllReplicas bool, populateReparentJournal bool) ([]*topodatapb.Tablet, error) {

	var replicasStartedReplication []*topodatapb.Tablet
	var replicaMutex sync.Mutex

	replCtx, replCancel := context.WithTimeout(ctx, opts.WaitReplicasTimeout)

	event.DispatchUpdate(ev, "reparenting all tablets")

	// Create a context and cancel function to watch for the first successful
	// SetMaster call on a replica. We use a background context so that this
	// context is only ever Done when its cancel is called by the background
	// goroutine we're about to spin up.
	//
	// Similarly, create a context and cancel for the replica waiter goroutine
	// to signal when all replica goroutines have finished. In the case where at
	// least one replica succeeds, replSuccessCtx will be canceled first, while
	// allReplicasDoneCtx is guaranteed to be canceled within
	// opts.WaitReplicasTimeout plus some jitter.
	replSuccessCtx, replSuccessCancel := context.WithCancel(context.Background())
	allReplicasDoneCtx, allReplicasDoneCancel := context.WithCancel(context.Background())

	now := time.Now().UnixNano()
	replWg := sync.WaitGroup{}
	rec := concurrency.AllErrorRecorder{}

	handlePrimary := func(alias string, tablet *topodatapb.Tablet) error {
		position, err := erp.tmc.MasterPosition(replCtx, tablet)
		if err != nil {
			return err
		}
		if populateReparentJournal {
			erp.logger.Infof("populating reparent journal on new primary %v", alias)
			return erp.tmc.PopulateReparentJournal(replCtx, tablet, now, opts.lockAction, newPrimaryTablet.Alias, position)
		}
		return nil
	}

	handleReplica := func(alias string, ti *topo.TabletInfo) {
		defer replWg.Done()
		erp.logger.Infof("setting new primary on replica %v", alias)

		forceStart := false
		if status, ok := statusMap[alias]; ok {
			fs, err := ReplicaWasRunning(status)
			if err != nil {
				err = vterrors.Wrapf(err, "tablet %v could not determine StopReplicationStatus: %v", alias, err)
				rec.RecordError(err)

				return
			}

			forceStart = fs
		}

		err := erp.tmc.SetMaster(replCtx, ti.Tablet, newPrimaryTablet.Alias, 0, "", forceStart)
		if err != nil {
			err = vterrors.Wrapf(err, "tablet %v SetReplicationSource failed: %v", alias, err)
			rec.RecordError(err)

			return
		}

		replicaMutex.Lock()
		replicasStartedReplication = append(replicasStartedReplication, ti.Tablet)
		replicaMutex.Unlock()

		// Signal that at least one goroutine succeeded to SetReplicationSource.
		// We do this only when we do not want to wait for all the replicas
		if !waitForAllReplicas {
			replSuccessCancel()
		}
	}

	numReplicas := 0

	for alias, ti := range tabletMap {
		switch {
		case alias == topoproto.TabletAliasString(newPrimaryTablet.Alias):
			continue
		case !opts.IgnoreReplicas.Has(alias):
			replWg.Add(1)
			numReplicas++
			go handleReplica(alias, ti)
		}
	}

	// Spin up a background goroutine to wait until all replica goroutines
	// finished. Polling this way allows us to have reparentReplicas return
	// success as soon as (a) the primary successfully populates its reparent
	// journal and (b) at least one replica successfully begins replicating.
	//
	// If we were to follow the more common pattern of blocking on replWg.Wait()
	// in the main body of promoteNewPrimary, we would be bound to the
	// time of slowest replica, instead of the time of the fastest successful
	// replica, and we want ERS to be fast.
	go func() {
		replWg.Wait()
		allReplicasDoneCancel()
	}()

	primaryErr := handlePrimary(topoproto.TabletAliasString(newPrimaryTablet.Alias), newPrimaryTablet)
	if primaryErr != nil {
		erp.logger.Warningf("primary failed to PopulateReparentJournal")
		replCancel()

		return nil, vterrors.Wrapf(primaryErr, "failed to PopulateReparentJournal on primary: %v", primaryErr)
	}

	go func() {
		replWg.Wait()
		defer replCancel()
	}()

	select {
	case <-replSuccessCtx.Done():
		// At least one replica was able to SetMaster successfully
		// Here we do not need to return the replicas which started replicating
		return nil, nil
	case <-allReplicasDoneCtx.Done():
		// There are certain timing issues between replSuccessCtx.Done firing
		// and allReplicasDoneCtx.Done firing, so we check again if truly all
		// replicas failed (where `numReplicas` goroutines recorded an error) or
		// one or more actually managed to succeed.
		errCount := len(rec.Errors)

		switch {
		case errCount > numReplicas:
			// Technically, rec.Errors should never be greater than numReplicas,
			// but it's better to err on the side of caution here, but also
			// we're going to be explicit that this is doubly unexpected.
			return nil, vterrors.Wrapf(rec.Error(), "received more errors (= %d) than replicas (= %d), which should be impossible: %v", errCount, numReplicas, rec.Error())
		case errCount == numReplicas:
			return nil, vterrors.Wrapf(rec.Error(), "%d replica(s) failed: %v", numReplicas, rec.Error())
		default:
			return replicasStartedReplication, nil
		}
	}

}

// intermediateCandidateIsIdeal is used to find whether the intermediate candidate that ERS chose is also the ideal one or not
func (erp *EmergencyReparenter) intermediateCandidateIsIdeal(newPrimary, prevPrimary *topodatapb.Tablet, validCandidates []*topodatapb.Tablet, tabletMap map[string]*topo.TabletInfo, opts EmergencyReparentOptions) (bool, error) {
	// we try to find a better candidate with the current list of valid candidates, and if it matches our current primary candidate, then we return true
	candidate, err := erp.identifyPrimaryCandidate(newPrimary, prevPrimary, validCandidates, tabletMap, opts)
	if err != nil {
		return false, err
	}
	return candidate == newPrimary, nil
}

// identifyPrimaryCandidate is used to find a better candidate for ERS promotion
func (erp *EmergencyReparenter) identifyPrimaryCandidate(newPrimary, prevPrimary *topodatapb.Tablet, validCandidates []*topodatapb.Tablet, tabletMap map[string]*topo.TabletInfo, opts EmergencyReparentOptions) (candidate *topodatapb.Tablet, err error) {
	defer func() {
		if candidate != nil {
			erp.logger.Infof("found better candidate - %v", candidate.Alias)
		}
	}()

	if opts.NewPrimaryAlias != nil {
		// explicit request to promote a specific tablet
		requestedPrimaryAlias := topoproto.TabletAliasString(opts.NewPrimaryAlias)
		requestedPrimaryInfo, isFound := tabletMap[requestedPrimaryAlias]
		if !isFound {
			return nil, vterrors.Errorf(vtrpc.Code_INTERNAL, "candidate %v not found in the tablet map; this an impossible situation", requestedPrimaryAlias)
		}
		for _, validCandidate := range validCandidates {
			if topoproto.TabletAliasEqual(validCandidate.Alias, opts.NewPrimaryAlias) {
				return requestedPrimaryInfo.Tablet, nil
			}
		}
		return nil, vterrors.Errorf(vtrpc.Code_ABORTED, "requested candidate %v is not in valid candidates list", requestedPrimaryAlias)
	}
	var preferredCandidates []*topodatapb.Tablet
	var neutralReplicas []*topodatapb.Tablet
	for _, candidate := range validCandidates {
		promotionRule := PromotionRule(candidate)
		if promotionRule == MustPromoteRule || promotionRule == PreferPromoteRule {
			preferredCandidates = append(preferredCandidates, candidate)
		}
		if promotionRule == NeutralPromoteRule {
			neutralReplicas = append(neutralReplicas, candidate)
		}
	}

	// So we've already promoted a replica.
	// However, can we improve on our choice? Are there any replicas with better promotion rules?
	// Maybe we actually promoted such a replica. Does that mean we should keep it?
	// Maybe we promoted a "neutral", and some "prefer" server is available.
	// Maybe we promoted a "prefer_not"
	// Maybe we promoted a server in a different cell than the primary
	// There's many options. We may wish to replace the server we promoted with a better one.

	// check whether the one we promoted is in the same cell and belongs to the preferred candidates list
	candidate = findPossibleCandidateFromListWithRestrictions(newPrimary, prevPrimary, preferredCandidates, true, true)
	if candidate != nil {
		return candidate, nil
	}
	// check whether there is some other tablet in the same cell belonging to the preferred candidates list
	candidate = findPossibleCandidateFromListWithRestrictions(newPrimary, prevPrimary, preferredCandidates, false, true)
	if candidate != nil {
		return candidate, nil
	}
	// we do not have a preferred candidate in the same cell

	if !opts.PreventCrossCellPromotion {
		// check whether the one we promoted belongs to the preferred candidates list
		candidate = findPossibleCandidateFromListWithRestrictions(newPrimary, prevPrimary, preferredCandidates, true, false)
		if candidate != nil {
			return candidate, nil
		}
		// check whether there is some other tablet belonging to the preferred candidates list
		candidate = findPossibleCandidateFromListWithRestrictions(newPrimary, prevPrimary, preferredCandidates, false, false)
		if candidate != nil {
			return candidate, nil
		}
	}

	// repeat the same process for the neutral candidates list
	candidate = findPossibleCandidateFromListWithRestrictions(newPrimary, prevPrimary, neutralReplicas, true, true)
	if candidate != nil {
		return candidate, nil
	}
	candidate = findPossibleCandidateFromListWithRestrictions(newPrimary, prevPrimary, neutralReplicas, false, true)
	if candidate != nil {
		return candidate, nil
	}

	if !opts.PreventCrossCellPromotion {
		candidate = findPossibleCandidateFromListWithRestrictions(newPrimary, prevPrimary, neutralReplicas, true, false)
		if candidate != nil {
			return candidate, nil
		}
		candidate = findPossibleCandidateFromListWithRestrictions(newPrimary, prevPrimary, neutralReplicas, false, false)
		if candidate != nil {
			return candidate, nil
		}
	}

	// return the one that we have if nothing found
	return newPrimary, nil
}

// checkIfConstraintsSatisfied is used to check whether the constraints for ERS are satisfied or not.
func (erp *EmergencyReparenter) checkIfConstraintsSatisfied(newPrimary, prevPrimary *topodatapb.Tablet, opts EmergencyReparentOptions) error {
	if opts.PreventCrossCellPromotion && prevPrimary != nil && newPrimary.Alias.Cell != prevPrimary.Alias.Cell {
		return vterrors.Errorf(vtrpc.Code_ABORTED, "elected primary does not satisfy geographic constraint - %s", topoproto.TabletAliasString(newPrimary.Alias))
	}
	if PromotionRule(newPrimary) == MustNotPromoteRule {
		return vterrors.Errorf(vtrpc.Code_ABORTED, "elected primary does not satisfy promotion rule constraint - %s", topoproto.TabletAliasString(newPrimary.Alias))
	}
	return nil
}

func (erp *EmergencyReparenter) promoteNewPrimary(ctx context.Context, ev *events.Reparent, newPrimary *topodatapb.Tablet, opts EmergencyReparentOptions, tabletMap map[string]*topo.TabletInfo, statusMap map[string]*replicationdatapb.StopReplicationStatus) error {
	erp.logger.Infof("starting promotion for the new primary - %v", newPrimary.Alias)
	// we call PromoteReplica which changes the tablet type, fixes the semi-sync, set the primary to read-write and flushes the binlogs
	_, err := erp.tmc.PromoteReplica(ctx, newPrimary)
	if err != nil {
		return vterrors.Wrapf(err, "primary-elect tablet %v failed to be upgraded to primary: %v", newPrimary.Alias, err)
	}
	// we now reparent all the replicas to the new primary we have promoted
	_, err = erp.reparentReplicas(ctx, ev, newPrimary, tabletMap, statusMap, opts, false, true)
	if err != nil {
		return err
	}
	return nil
}
