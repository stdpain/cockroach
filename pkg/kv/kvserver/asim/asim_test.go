// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package asim_test

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/asim"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/asim/state"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/asim/workload"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/stretchr/testify/require"
)

func TestRunAllocatorSimulator(t *testing.T) {
	ctx := context.Background()
	start := state.TestingStartTime()
	end := start.Add(1000 * time.Second)
	interval := 10 * time.Second
	rwg := make([]workload.Generator, 1)
	rwg[0] = testCreateWorkloadGenerator(start, 1, 10)
	m := asim.NewMetricsTracker(os.Stdout)
	exchange := state.NewFixedDelayExhange(start, interval, interval)
	changer := state.NewReplicaChanger()
	s := state.LoadConfig(state.ComplexConfig)

	sim := asim.NewSimulator(start, end, interval, rwg, s, exchange, changer, interval, m)
	sim.RunSim(ctx)
}

// testCreateWorkloadGenerator creates a simple uniform workload generator that
// will generate load events at a rate of 500 per store. The read ratio is
// fixed to 0.95.
func testCreateWorkloadGenerator(start time.Time, stores int, keySpan int64) workload.Generator {
	readRatio := 0.95
	minWriteSize := 128
	maxWriteSize := 256
	workloadRate := float64(stores * 500)
	r := rand.New(rand.NewSource(state.TestingWorkloadSeed()))

	return workload.NewRandomGenerator(
		start,
		state.TestingWorkloadSeed(),
		workload.NewUniformKeyGen(keySpan, r),
		workloadRate,
		readRatio,
		maxWriteSize,
		minWriteSize,
	)
}

// testPreGossipStores populates the state exchange with the existing state.
// This is done at the time given, which should be before the test start time
// minus the gossip delay and interval. This alleviates a cold start, where the
// allocator for each store does not have information to make a decision for
// the ranges it holds leases for.
func testPreGossipStores(s state.State, exchange state.Exchange, at time.Time) {
	storeDescriptors := s.StoreDescriptors()
	exchange.Put(at, storeDescriptors...)
}

// TestAllocatorSimulatorSpeed tests that the simulation runs at a rate of at
// least 5 simulated minutes per wall clock second (1:600) for a 12 node
// cluster, with 6000 replicas. The workload is generating 6000 keys per second
// with a uniform distribution.
// NB: In practice, on a single thread N2 GCP VM, this completes with a minimum
// run of 40ms, approximately 12x faster (1:14000) than what this test asserts.
// The limit is set much higher due to --stress and inconsistent processor
// speeds. The speedup is not linear w.r.t replica count.
// TODO(kvoli,lidorcarmel): If this test flakes on CI --stress --race, decrease
// the stores, or decrease replicasPerStore.
func TestAllocatorSimulatorSpeed(t *testing.T) {
	ctx := context.Background()
	start := state.TestingStartTime()

	// Run each simulation for 5 minutes.
	end := start.Add(5 * time.Minute)
	interval := 10 * time.Second
	changeDelay := 5 * time.Second
	gossipDelay := 100 * time.Millisecond
	preGossipStart := start.Add(-interval - gossipDelay)

	stores := 4
	replsPerRange := 3
	replicasPerStore := 300
	// NB: We want 500 replicas per store, so the number of ranges required
	// will be 1/3 of the total replicas.
	ranges := (replicasPerStore * stores) / replsPerRange

	sample := func() int64 {
		rwg := make([]workload.Generator, 1)
		rwg[0] = testCreateWorkloadGenerator(start, stores, int64(ranges))
		exchange := state.NewFixedDelayExhange(preGossipStart, interval, gossipDelay)
		changer := state.NewReplicaChanger()
		m := asim.NewMetricsTracker() // no output
		replicaDistribution := make([]float64, stores)

		// NB: Here create half of the stores with equal replica counts, the
		// other half have no replicas. This will lead to a flurry of activity
		// rebalancing towards these stores, based on the replica count
		// imbalance.
		for i := 0; i < stores/2; i++ {
			replicaDistribution[i] = 1.0 / float64(stores/2)
		}
		for i := stores / 2; i < stores; i++ {
			replicaDistribution[i] = 0
		}

		s := state.NewTestStateReplDistribution(ranges, replicaDistribution, replsPerRange)
		testPreGossipStores(s, exchange, preGossipStart)
		sim := asim.NewSimulator(start, end, interval, rwg, s, exchange, changer, changeDelay, m)

		startTime := timeutil.Now()
		sim.RunSim(ctx)
		return timeutil.Since(startTime).Nanoseconds()
	}

	// We sample 5 runs and take the minimum. The minimum is the cleanest
	// estimate here of performance, as any additional time over the minimum is
	// noise in a run.
	minRunTime := int64(math.MaxInt64)
	samples := 5
	for i := 0; i < samples; i++ {
		if sampledRun := sample(); sampledRun < minRunTime {
			minRunTime = sampledRun
		}
	}

	fmt.Println(time.Duration(minRunTime).Seconds())
	// TODO(lidor,kvoli): in CI this test takes many seconds, we need to optimize.
	require.Less(t, minRunTime, 20*time.Second.Nanoseconds())
}
