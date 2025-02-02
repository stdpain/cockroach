// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package stmtdiagnostics

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"time"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlutil"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

var pollingInterval = settings.RegisterDurationSetting(
	settings.TenantWritable,
	"sql.stmt_diagnostics.poll_interval",
	"rate at which the stmtdiagnostics.Registry polls for requests, set to zero to disable",
	10*time.Second)

var bundleChunkSize = settings.RegisterByteSizeSetting(
	settings.TenantWritable,
	"sql.stmt_diagnostics.bundle_chunk_size",
	"chunk size for statement diagnostic bundles",
	1024*1024,
	func(val int64) error {
		if val < 16 {
			return errors.Errorf("chunk size must be at least 16 bytes")
		}
		return nil
	},
)

// Registry maintains a view on the statement fingerprints
// on which data is to be collected (i.e. system.statement_diagnostics_requests)
// and provides utilities for checking a query against this list and satisfying
// the requests.
type Registry struct {
	mu struct {
		// NOTE: This lock can't be held while the registry runs any statements
		// internally; it'd deadlock.
		syncutil.Mutex
		// requests waiting for the right query to come along. The conditional
		// requests are left in this map until they either are satisfied or
		// expire (i.e. they never enter unconditionalOngoing map).
		requestFingerprints map[RequestID]Request
		// ids of unconditional requests that this node is in the process of
		// servicing.
		unconditionalOngoing map[RequestID]Request

		// epoch is observed before reading system.statement_diagnostics_requests, and then
		// checked again before loading the tables contents. If the value changed in
		// between, then the table contents might be stale.
		epoch int

		rand *rand.Rand
	}
	st     *cluster.Settings
	ie     sqlutil.InternalExecutor
	db     *kv.DB
	gossip gossip.OptionalGossip

	// gossipUpdateChan is used to notify the polling loop that a diagnostics
	// request has been added. The gossip callback will not block sending on this
	// channel.
	gossipUpdateChan chan RequestID
	// gossipCancelChan is used to notify the polling loop that a diagnostics
	// request has been canceled. The gossip callback will not block sending on
	// this channel.
	gossipCancelChan chan RequestID
}

// Request describes a statement diagnostics request along with some conditional
// information.
type Request struct {
	fingerprint         string
	samplingProbability float64
	minExecutionLatency time.Duration
	expiresAt           time.Time
}

func (r *Request) isExpired(now time.Time) bool {
	return !r.expiresAt.IsZero() && r.expiresAt.Before(now)
}

func (r *Request) isConditional() bool {
	return r.minExecutionLatency != 0
}

// NewRegistry constructs a new Registry.
func NewRegistry(
	ie sqlutil.InternalExecutor, db *kv.DB, gw gossip.OptionalGossip, st *cluster.Settings,
) *Registry {
	r := &Registry{
		ie:               ie,
		db:               db,
		gossip:           gw,
		gossipUpdateChan: make(chan RequestID, 1),
		gossipCancelChan: make(chan RequestID, 1),
		st:               st,
	}
	r.mu.rand = rand.New(rand.NewSource(timeutil.Now().UnixNano()))

	// Some tests pass a nil gossip, and gossip is not available on SQL tenant
	// servers.
	g, ok := gw.Optional(47893)
	if ok && g != nil {
		g.RegisterCallback(gossip.KeyGossipStatementDiagnosticsRequest, r.gossipNotification)
	}
	return r
}

// Start will start the polling loop for the Registry.
func (r *Registry) Start(ctx context.Context, stopper *stop.Stopper) {
	ctx, _ = stopper.WithCancelOnQuiesce(ctx)
	// NB: The only error that should occur here would be if the server were
	// shutting down so let's swallow it.
	_ = stopper.RunAsyncTask(ctx, "stmt-diag-poll", r.poll)
}

func (r *Registry) poll(ctx context.Context) {
	var (
		timer               timeutil.Timer
		lastPoll            time.Time
		deadline            time.Time
		pollIntervalChanged = make(chan struct{}, 1)
		maybeResetTimer     = func() {
			if interval := pollingInterval.Get(&r.st.SV); interval <= 0 {
				// Setting the interval to a non-positive value stops the polling.
				timer.Stop()
			} else {
				newDeadline := lastPoll.Add(interval)
				if deadline.IsZero() || !deadline.Equal(newDeadline) {
					deadline = newDeadline
					timer.Reset(timeutil.Until(deadline))
				}
			}
		}
		poll = func() {
			if err := r.pollRequests(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Warningf(ctx, "error polling for statement diagnostics requests: %s", err)
			}
			lastPoll = timeutil.Now()
		}
	)
	pollingInterval.SetOnChange(&r.st.SV, func(ctx context.Context) {
		select {
		case pollIntervalChanged <- struct{}{}:
		default:
		}
	})
	for {
		maybeResetTimer()
		select {
		case <-pollIntervalChanged:
			continue // go back around and maybe reset the timer
		case reqID := <-r.gossipUpdateChan:
			if r.findRequest(reqID) {
				continue // request already exists, don't do anything
			}
			// Poll the data.
		case reqID := <-r.gossipCancelChan:
			r.cancelRequest(reqID)
			// No need to poll the data (unlike above) because we don't have to
			// read anything of the system table to remove the request from the
			// registry.
			continue
		case <-timer.C:
			timer.Read = true
		case <-ctx.Done():
			return
		}
		poll()
	}
}

// RequestID is the ID of a diagnostics request, corresponding to the id
// column in statement_diagnostics_requests.
// A zero ID is invalid.
type RequestID int

// CollectedInstanceID is the ID of an instance of collected diagnostics,
// corresponding to the id column in statement_diagnostics.
type CollectedInstanceID int

// addRequestInternalLocked adds a request to r.mu.requestFingerprints. If the
// request is already present or it has already expired, the call is a noop.
func (r *Registry) addRequestInternalLocked(
	ctx context.Context,
	id RequestID,
	queryFingerprint string,
	samplingProbability float64,
	minExecutionLatency time.Duration,
	expiresAt time.Time,
) {
	if r.findRequestLocked(id) {
		// Request already exists.
		return
	}
	if r.mu.requestFingerprints == nil {
		r.mu.requestFingerprints = make(map[RequestID]Request)
	}
	r.mu.requestFingerprints[id] = Request{
		fingerprint:         queryFingerprint,
		samplingProbability: samplingProbability,
		minExecutionLatency: minExecutionLatency,
		expiresAt:           expiresAt,
	}
}

func (r *Registry) findRequest(requestID RequestID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.findRequestLocked(requestID)
}

// findRequestLocked returns whether the request already exists. If the request
// is not ongoing and has already expired, it is removed from the registry (yet
// true is still returned).
func (r *Registry) findRequestLocked(requestID RequestID) bool {
	f, ok := r.mu.requestFingerprints[requestID]
	if ok {
		if f.isExpired(timeutil.Now()) {
			// This request has already expired.
			delete(r.mu.requestFingerprints, requestID)
		}
		return true
	}
	_, ok = r.mu.unconditionalOngoing[requestID]
	return ok
}

// cancelRequest removes the request with the given RequestID from the Registry
// if present.
func (r *Registry) cancelRequest(requestID RequestID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.mu.requestFingerprints, requestID)
	delete(r.mu.unconditionalOngoing, requestID)
}

// InsertRequest is part of the StmtDiagnosticsRequester interface.
func (r *Registry) InsertRequest(
	ctx context.Context,
	stmtFingerprint string,
	samplingProbability float64,
	minExecutionLatency time.Duration,
	expiresAfter time.Duration,
) error {
	_, err := r.insertRequestInternal(ctx, stmtFingerprint, samplingProbability, minExecutionLatency, expiresAfter)
	return err
}

func (r *Registry) insertRequestInternal(
	ctx context.Context,
	stmtFingerprint string,
	samplingProbability float64,
	minExecutionLatency time.Duration,
	expiresAfter time.Duration,
) (RequestID, error) {
	g, err := r.gossip.OptionalErr(48274)
	if err != nil {
		return 0, err
	}

	isSamplingProbabilitySupported := r.st.Version.IsActive(ctx, clusterversion.SampledStmtDiagReqs)
	if !isSamplingProbabilitySupported && samplingProbability != 0 {
		return 0, errors.New(
			"sampling probability only supported after 22.2 version migrations have completed",
		)
	}
	if samplingProbability < 0 || samplingProbability > 1 {
		return 0, errors.AssertionFailedf(
			"malformed input: expected sampling probability in range [0.0, 1.0], got %f",
			samplingProbability)
	}
	if samplingProbability != 0 && minExecutionLatency.Nanoseconds() == 0 {
		return 0, errors.AssertionFailedf(
			"malformed input: got non-zero sampling probability %f and empty min exec latency",
			samplingProbability)
	}

	var reqID RequestID
	var expiresAt time.Time
	err = r.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		// Check if there's already a pending request for this fingerprint.
		row, err := r.ie.QueryRowEx(ctx, "stmt-diag-check-pending", txn,
			sessiondata.InternalExecutorOverride{
				User: username.RootUserName(),
			},
			`SELECT count(1) FROM system.statement_diagnostics_requests
				WHERE
					completed = false AND
					statement_fingerprint = $1 AND
					(expires_at IS NULL OR expires_at > now())`,
			stmtFingerprint)
		if err != nil {
			return err
		}
		if row == nil {
			return errors.New("failed to check pending statement diagnostics")
		}
		count := int(*row[0].(*tree.DInt))
		if count != 0 {
			return errors.New(
				"A pending request for the requested fingerprint already exists. " +
					"Cancel the existing request first and try again.",
			)
		}

		now := timeutil.Now()
		insertColumns := "statement_fingerprint, requested_at"
		qargs := make([]interface{}, 2, 5)
		qargs[0] = stmtFingerprint // statement_fingerprint
		qargs[1] = now             // requested_at
		if samplingProbability != 0 {
			insertColumns += ", sampling_probability"
			qargs = append(qargs, samplingProbability) // sampling_probability
		}
		if minExecutionLatency != 0 {
			insertColumns += ", min_execution_latency"
			qargs = append(qargs, minExecutionLatency) // min_execution_latency
		}
		if expiresAfter != 0 {
			insertColumns += ", expires_at"
			expiresAt = now.Add(expiresAfter)
			qargs = append(qargs, expiresAt) // expires_at
		}
		valuesClause := "$1, $2"
		for i := range qargs[2:] {
			valuesClause += fmt.Sprintf(", $%d", i+3)
		}
		stmt := "INSERT INTO system.statement_diagnostics_requests (" +
			insertColumns + ") VALUES (" + valuesClause + ") RETURNING id;"
		row, err = r.ie.QueryRowEx(
			ctx, "stmt-diag-insert-request", txn,
			sessiondata.InternalExecutorOverride{User: username.RootUserName()},
			stmt, qargs...,
		)
		if err != nil {
			return err
		}
		if row == nil {
			return errors.New("failed to insert statement diagnostics request")
		}
		reqID = RequestID(*row[0].(*tree.DInt))
		return nil
	})
	if err != nil {
		return 0, err
	}

	// Manually insert the request in the (local) registry. This lets this node
	// pick up the request quickly if the right query comes around, without
	// waiting for the poller.
	r.mu.Lock()
	r.mu.epoch++
	r.addRequestInternalLocked(ctx, reqID, stmtFingerprint, samplingProbability, minExecutionLatency, expiresAt)
	r.mu.Unlock()

	// Notify all the other nodes that they have to poll.
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(reqID))
	if err := g.AddInfo(gossip.KeyGossipStatementDiagnosticsRequest, buf, 0 /* ttl */); err != nil {
		log.Warningf(ctx, "error notifying of diagnostics request: %s", err)
	}

	return reqID, nil
}

// CancelRequest is part of the server.StmtDiagnosticsRequester interface.
func (r *Registry) CancelRequest(ctx context.Context, requestID int64) error {
	g, err := r.gossip.OptionalErr(48274)
	if err != nil {
		return err
	}

	row, err := r.ie.QueryRowEx(ctx, "stmt-diag-cancel-request", nil, /* txn */
		sessiondata.InternalExecutorOverride{
			User: username.RootUserName(),
		},
		// Rather than deleting the row from the table, we choose to mark the
		// request as "expired" by setting `expires_at` into the past. This will
		// allow any queries that are currently being traced for this request to
		// write their collected bundles.
		"UPDATE system.statement_diagnostics_requests SET expires_at = '1970-01-01' "+
			"WHERE completed = false AND id = $1 "+
			"AND (expires_at IS NULL OR expires_at > now()) RETURNING id;",
		requestID,
	)
	if err != nil {
		return err
	}

	if row == nil {
		// There is no pending diagnostics request with the given fingerprint.
		return errors.Newf("no pending request found for the fingerprint: %s", requestID)
	}

	reqID := RequestID(requestID)
	r.cancelRequest(reqID)

	// Notify all the other nodes that this request has been canceled.
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(reqID))
	if err := g.AddInfo(gossip.KeyGossipStatementDiagnosticsRequestCancellation, buf, 0 /* ttl */); err != nil {
		log.Warningf(ctx, "error notifying of diagnostics request cancellation: %s", err)
	}

	return nil
}

// IsExecLatencyConditionMet returns true if the completed request's execution
// latency satisfies the request's condition. If false is returned, it inlines
// the logic of RemoveOngoing.
func (r *Registry) IsExecLatencyConditionMet(
	requestID RequestID, req Request, execLatency time.Duration,
) bool {
	if req.minExecutionLatency <= execLatency {
		return true
	}
	// This is a conditional request and the condition is not satisfied, so we
	// only need to remove the request if it has expired.
	if req.isExpired(timeutil.Now()) {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.mu.requestFingerprints, requestID)
	}
	return false
}

// RemoveOngoing removes the given request from the list of ongoing queries.
func (r *Registry) RemoveOngoing(requestID RequestID, req Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.isConditional() {
		if req.isExpired(timeutil.Now()) {
			delete(r.mu.requestFingerprints, requestID)
		}
	} else {
		delete(r.mu.unconditionalOngoing, requestID)
	}
}

// ShouldCollectDiagnostics checks whether any data should be collected for the
// given query, which is the case if the registry has a request for this
// statement's fingerprint (and assuming probability conditions hold); in this
// case ShouldCollectDiagnostics will return true again on this node for the
// same diagnostics request only for conditional requests.
//
// If shouldCollect is true, RemoveOngoing needs to be called (which is inlined
// by IsExecLatencyConditionMet when that returns false).
func (r *Registry) ShouldCollectDiagnostics(
	ctx context.Context, fingerprint string,
) (shouldCollect bool, reqID RequestID, req Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Return quickly if we have no requests to trace.
	if len(r.mu.requestFingerprints) == 0 {
		return false, 0, req
	}

	for id, f := range r.mu.requestFingerprints {
		if f.fingerprint == fingerprint {
			if f.isExpired(timeutil.Now()) {
				delete(r.mu.requestFingerprints, id)
				return false, 0, req
			}
			reqID = id
			req = f
			break
		}
	}

	if reqID == 0 {
		return false, 0, Request{}
	}

	if !req.isConditional() {
		if r.mu.unconditionalOngoing == nil {
			r.mu.unconditionalOngoing = make(map[RequestID]Request)
		}
		r.mu.unconditionalOngoing[reqID] = req
		delete(r.mu.requestFingerprints, reqID)
	}

	if req.samplingProbability == 0 || r.mu.rand.Float64() < req.samplingProbability {
		return true, reqID, req
	}
	return false, 0, Request{}
}

// InsertStatementDiagnostics inserts a trace into system.statement_diagnostics.
//
// traceJSON is either DNull (when collectionErr should not be nil) or a *DJSON.
//
// If requestID is not zero, it also marks the request as completed in
// system.statement_diagnostics_requests. If requestID is zero, a new entry is
// inserted.
//
// collectionErr should be any error generated during the collection or
// generation of the bundle/trace.
func (r *Registry) InsertStatementDiagnostics(
	ctx context.Context,
	requestID RequestID,
	stmtFingerprint string,
	stmt string,
	bundle []byte,
	collectionErr error,
) (CollectedInstanceID, error) {
	var diagID CollectedInstanceID
	err := r.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		if requestID != 0 {
			row, err := r.ie.QueryRowEx(ctx, "stmt-diag-check-completed", txn,
				sessiondata.InternalExecutorOverride{User: username.RootUserName()},
				"SELECT count(1) FROM system.statement_diagnostics_requests WHERE id = $1 AND completed = false",
				requestID)
			if err != nil {
				return err
			}
			if row == nil {
				return errors.New("failed to check completed statement diagnostics")
			}
			cnt := int(*row[0].(*tree.DInt))
			if cnt == 0 {
				// Someone else already marked the request as completed. We've traced for nothing.
				// This can only happen once per node, per request since we're going to
				// remove the request from the registry.
				return nil
			}
		}

		// Generate the values that will be inserted.
		errorVal := tree.DNull
		if collectionErr != nil {
			errorVal = tree.NewDString(collectionErr.Error())
		}

		bundleChunksVal := tree.NewDArray(types.Int)
		for len(bundle) > 0 {
			chunkSize := int(bundleChunkSize.Get(&r.st.SV))
			chunk := bundle
			if len(chunk) > chunkSize {
				chunk = chunk[:chunkSize]
			}
			bundle = bundle[len(chunk):]

			// Insert the chunk into system.statement_bundle_chunks.
			row, err := r.ie.QueryRowEx(
				ctx, "stmt-bundle-chunks-insert", txn,
				sessiondata.InternalExecutorOverride{User: username.RootUserName()},
				"INSERT INTO system.statement_bundle_chunks(description, data) VALUES ($1, $2) RETURNING id",
				"statement diagnostics bundle",
				tree.NewDBytes(tree.DBytes(chunk)),
			)
			if err != nil {
				return err
			}
			if row == nil {
				return errors.New("failed to check statement bundle chunk")
			}
			chunkID := row[0].(*tree.DInt)
			if err := bundleChunksVal.Append(chunkID); err != nil {
				return err
			}
		}

		collectionTime := timeutil.Now()

		// Insert the trace into system.statement_diagnostics.
		row, err := r.ie.QueryRowEx(
			ctx, "stmt-diag-insert", txn,
			sessiondata.InternalExecutorOverride{User: username.RootUserName()},
			"INSERT INTO system.statement_diagnostics "+
				"(statement_fingerprint, statement, collected_at, bundle_chunks, error) "+
				"VALUES ($1, $2, $3, $4, $5) RETURNING id",
			stmtFingerprint, stmt, collectionTime, bundleChunksVal, errorVal,
		)
		if err != nil {
			return err
		}
		if row == nil {
			return errors.New("failed to insert statement diagnostics")
		}
		diagID = CollectedInstanceID(*row[0].(*tree.DInt))

		if requestID != 0 {
			// Mark the request from system.statement_diagnostics_request as completed.
			_, err := r.ie.ExecEx(ctx, "stmt-diag-mark-completed", txn,
				sessiondata.InternalExecutorOverride{User: username.RootUserName()},
				"UPDATE system.statement_diagnostics_requests "+
					"SET completed = true, statement_diagnostics_id = $1 WHERE id = $2",
				diagID, requestID)
			if err != nil {
				return err
			}
		} else {
			// Insert a completed request into system.statement_diagnostics_request.
			// This is necessary because the UI uses this table to discover completed
			// diagnostics.
			_, err := r.ie.ExecEx(ctx, "stmt-diag-add-completed", txn,
				sessiondata.InternalExecutorOverride{User: username.RootUserName()},
				"INSERT INTO system.statement_diagnostics_requests"+
					" (completed, statement_fingerprint, statement_diagnostics_id, requested_at)"+
					" VALUES (true, $1, $2, $3)",
				stmtFingerprint, diagID, collectionTime)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return diagID, nil
}

// pollRequests reads the pending rows from system.statement_diagnostics_requests and
// updates r.mu.requests accordingly.
func (r *Registry) pollRequests(ctx context.Context) error {
	var rows []tree.Datums
	isSamplingProbabilitySupported := r.st.Version.IsActive(ctx, clusterversion.SampledStmtDiagReqs)

	// Loop until we run the query without straddling an epoch increment.
	for {
		r.mu.Lock()
		epoch := r.mu.epoch
		r.mu.Unlock()

		var extraColumns string
		if isSamplingProbabilitySupported {
			extraColumns = ", sampling_probability"
		}
		it, err := r.ie.QueryIteratorEx(ctx, "stmt-diag-poll", nil, /* txn */
			sessiondata.InternalExecutorOverride{
				User: username.RootUserName(),
			},
			fmt.Sprintf(`SELECT id, statement_fingerprint, min_execution_latency, expires_at%s
				FROM system.statement_diagnostics_requests
				WHERE completed = false AND (expires_at IS NULL OR expires_at > now())`, extraColumns),
		)
		if err != nil {
			return err
		}
		rows = rows[:0]
		var ok bool
		for ok, err = it.Next(ctx); ok; ok, err = it.Next(ctx) {
			rows = append(rows, it.Cur())
		}
		if err != nil {
			return err
		}

		r.mu.Lock()
		// If the epoch changed it means that a request was added to the registry
		// manually while the query was running. In that case, if we were to process
		// the query results normally, we might remove that manually-added request.
		if r.mu.epoch != epoch {
			r.mu.Unlock()
			continue
		}
		break
	}
	defer r.mu.Unlock()

	now := timeutil.Now()
	var ids util.FastIntSet
	for _, row := range rows {
		id := RequestID(*row[0].(*tree.DInt))
		stmtFingerprint := string(*row[1].(*tree.DString))
		var minExecutionLatency time.Duration
		var expiresAt time.Time
		var samplingProbability float64

		if minExecLatency, ok := row[2].(*tree.DInterval); ok {
			minExecutionLatency = time.Duration(minExecLatency.Nanos())
		}
		if e, ok := row[3].(*tree.DTimestampTZ); ok {
			expiresAt = e.Time
		}
		if isSamplingProbabilitySupported {
			if prob, ok := row[4].(*tree.DFloat); ok {
				samplingProbability = float64(*prob)
			}
		}
		ids.Add(int(id))
		r.addRequestInternalLocked(ctx, id, stmtFingerprint, samplingProbability, minExecutionLatency, expiresAt)
	}

	// Remove all other requests.
	for id, req := range r.mu.requestFingerprints {
		if !ids.Contains(int(id)) || req.isExpired(now) {
			delete(r.mu.requestFingerprints, id)
		}
	}
	return nil
}

// gossipNotification is called in response to a gossip update informing us that
// we need to poll.
func (r *Registry) gossipNotification(s string, value roachpb.Value) {
	switch s {
	case gossip.KeyGossipStatementDiagnosticsRequest:
		select {
		case r.gossipUpdateChan <- RequestID(binary.LittleEndian.Uint64(value.RawBytes)):
		default:
			// Don't pile up on these requests and don't block gossip.
		}
	case gossip.KeyGossipStatementDiagnosticsRequestCancellation:
		select {
		case r.gossipCancelChan <- RequestID(binary.LittleEndian.Uint64(value.RawBytes)):
		default:
			// Don't pile up on these requests and don't block gossip.
		}
	default:
		// We don't expect any other notifications. Perhaps in a future version
		// we added other keys with the same prefix.
		return
	}
}
