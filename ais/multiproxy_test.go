// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/devtools/tutils"
	"github.com/NVIDIA/aistore/stats"
	jsoniter "github.com/json-iterator/go"
)

const httpProto = "http"

type (
	discoverServerHandler func(sv int64, lv int64) *httptest.Server

	discoverServer struct {
		id          string
		isProxy     bool
		smapVersion int64
		bmdVersion  int64
		httpHandler discoverServerHandler
	}
)

// newDiscoverServerPrimary returns a proxy runner after initializing the fields that are needed by this test
func newDiscoverServerPrimary() *proxyrunner {
	var (
		p       = &proxyrunner{}
		tracker = stats.NewTrackerMock()
	)
	p.si = cluster.NewSnode("primary", httpProto, cmn.Proxy, &net.TCPAddr{}, &net.TCPAddr{}, &net.TCPAddr{})
	p.client.data = &http.Client{}
	p.client.control = &http.Client{}

	config := cmn.GCO.BeginUpdate()
	config.Keepalive.Proxy.Name = "heartbeat"
	config.Timeout.Startup = 4 * time.Second
	config.Timeout.CplaneOperation = 2 * time.Second
	config.Timeout.MaxKeepalive = 4 * time.Second
	config.Client.Timeout = 10 * time.Second
	config.Client.TimeoutLong = 10 * time.Second
	cmn.GCO.CommitUpdate(config)

	p.owner.smap = newSmapOwner()
	p.owner.smap.put(newSmap())
	owner := newBMDOwnerPrx(config)
	owner._put(newBucketMD())
	p.owner.bmd = owner
	p.keepalive = newProxyKeepaliveRunner(p, tracker, atomic.NewBool(true))
	return p
}

// discoverServerDefaultHandler returns the Smap and BMD with the given version
func discoverServerDefaultHandler(sv, lv int64) *httptest.Server {
	smapVersion := sv
	bmdVersion := lv
	return httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			msg := SmapVoteMsg{
				VoteInProgress: false,
				Smap:           &smapX{cluster.Smap{Version: smapVersion}},
				BMD:            &bucketMD{BMD: cluster.BMD{Version: bmdVersion}},
			}
			b, _ := jsoniter.Marshal(msg)
			w.Write(b)
		},
	))
}

// discoverServerVoteOnceHandler returns vote in progress on the first time it is call, returns
// Smap and BMD on subsequent calls
func discoverServerVoteOnceHandler(sv, lv int64) *httptest.Server {
	cnt := 0
	smapVersion := sv
	bmdVersion := lv
	f := func(w http.ResponseWriter, r *http.Request) {
		cnt++
		msg := SmapVoteMsg{
			VoteInProgress: cnt == 1,
			Smap:           &smapX{cluster.Smap{Version: smapVersion}},
			BMD:            &bucketMD{BMD: cluster.BMD{Version: bmdVersion}},
		}
		b, _ := jsoniter.Marshal(msg)
		w.Write(b)
	}

	return httptest.NewServer(http.HandlerFunc(f))
}

// discoverServerFailTwiceHandler fails the first two calls and returns
// Smap abd BMD on subsequent calls
func discoverServerFailTwiceHandler(sv, lv int64) *httptest.Server {
	cnt := 0
	smapVersion := sv
	bmdVersion := lv
	f := func(w http.ResponseWriter, r *http.Request) {
		cnt++
		if cnt > 2 {
			msg := SmapVoteMsg{
				VoteInProgress: false,
				Smap:           &smapX{cluster.Smap{Version: smapVersion}},
				BMD:            &bucketMD{BMD: cluster.BMD{Version: bmdVersion}},
			}
			b, _ := jsoniter.Marshal(msg)
			w.Write(b)
		} else {
			http.Error(w, "retry", http.StatusUnavailableForLegalReasons)
		}
	}

	return httptest.NewServer(http.HandlerFunc(f))
}

// discoverServerAlwaysFailHandler always responds with error
func discoverServerAlwaysFailHandler(sv, lv int64) *httptest.Server {
	f := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "retry", http.StatusUnavailableForLegalReasons)
	}

	return httptest.NewServer(http.HandlerFunc(f))
}

// discoverServerVoteInProgressHandler always responds with vote in progress
func discoverServerVoteInProgressHandler(sv, lv int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			msg := SmapVoteMsg{
				VoteInProgress: true,
				Smap:           &smapX{cluster.Smap{Version: 12345}},
				BMD:            &bucketMD{BMD: cluster.BMD{Version: 67890}},
			}
			b, _ := jsoniter.Marshal(msg)
			w.Write(b)
		},
	))
}

func TestDiscoverServers(t *testing.T) {
	tcs := []struct {
		name        string
		servers     []discoverServer
		duration    time.Duration // how long to wait for discover servers call
		smapVersion int64         // expected return from discover servers
		bmdVersion  int64         // use '0' if expecting nil Smap or BMD
		onlyLong    bool          // run only in long mode
	}{
		{
			"empty discovery Smap",
			[]discoverServer{},
			time.Millisecond,
			0,
			0,
			false,
		},
		{
			"all agreed",
			[]discoverServer{
				{"p1", true, 1, 2, discoverServerDefaultHandler},
				{"t1", false, 1, 2, discoverServerDefaultHandler},
			},
			time.Millisecond,
			1,
			2,
			false,
		},
		{
			"mixed",
			[]discoverServer{
				{"p1", true, 1, 2, discoverServerDefaultHandler},
				{"t1", false, 4, 5, discoverServerDefaultHandler},
				{"t2", false, 1, 2, discoverServerDefaultHandler},
			},
			time.Millisecond,
			4,
			5,
			false,
		},
		{
			"voting",
			[]discoverServer{
				{"t1", false, 4, 5, discoverServerVoteInProgressHandler},
				{"t2", false, 1, 2, discoverServerVoteInProgressHandler},
			},
			time.Millisecond * 300,
			0,
			0,
			true,
		},
		{
			"voting and map mixed",
			[]discoverServer{
				{"t1", false, 4, 5, discoverServerVoteInProgressHandler},
				{"t2", false, 1, 2, discoverServerDefaultHandler},
			},
			time.Millisecond * 300,
			0,
			0,
			true,
		},
		{
			"vote once",
			[]discoverServer{
				{"t1", false, 4, 5, discoverServerVoteOnceHandler},
				{"t2", false, 1, 2, discoverServerDefaultHandler},
			},
			time.Millisecond * 3000,
			4,
			5,
			false,
		},
		{
			"fail twice",
			[]discoverServer{
				{"t1", false, 4, 5, discoverServerFailTwiceHandler},
				{"t2", false, 1, 2, discoverServerDefaultHandler},
			},
			time.Millisecond * 3000,
			4,
			5,
			true,
		},
		{
			"all failed",
			[]discoverServer{
				{"t1", false, 4, 5, discoverServerAlwaysFailHandler},
				{"t2", false, 1, 2, discoverServerAlwaysFailHandler},
			},
			time.Millisecond * 400,
			0,
			0,
			true,
		},
		{
			"fail and good mixed",
			[]discoverServer{
				{"t1", false, 4, 5, discoverServerDefaultHandler},
				{"t2", false, 1, 2, discoverServerAlwaysFailHandler},
			},
			time.Millisecond * 400,
			4,
			5,
			true,
		},
		{
			"zero Smap version",
			[]discoverServer{
				{"p1", true, 0, 3, discoverServerDefaultHandler},
				{"t1", false, 0, 4, discoverServerDefaultHandler},
			},
			time.Millisecond * 400,
			0,
			4,
			false,
		},
		{
			"zero BMD version",
			[]discoverServer{
				{"p1", true, 1, 0, discoverServerDefaultHandler},
				{"t1", false, 1, 0, discoverServerDefaultHandler},
			},
			time.Millisecond * 400,
			1,
			0,
			false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			tutils.CheckSkip(t, tutils.SkipTestArgs{Long: tc.onlyLong})
			var (
				primary      = newDiscoverServerPrimary()
				discoverSmap = newSmap()
			)

			for _, s := range tc.servers {
				ts := s.httpHandler(s.smapVersion, s.bmdVersion)
				addrInfo := serverTCPAddr(ts.URL)
				if s.isProxy {
					discoverSmap.addProxy(cluster.NewSnode(s.id, httpProto, cmn.Proxy, addrInfo, &net.TCPAddr{}, &net.TCPAddr{}))
				} else {
					discoverSmap.addTarget(cluster.NewSnode(s.id, httpProto, cmn.Target, addrInfo, &net.TCPAddr{}, &net.TCPAddr{}))
				}
			}
			svm := primary.uncoverMeta(discoverSmap)
			if tc.smapVersion == 0 {
				if svm.Smap != nil && svm.Smap.version() > 0 {
					t.Errorf("test case %q: expecting nil Smap", tc.name)
				}
			} else {
				if svm.Smap == nil || svm.Smap.version() == 0 {
					t.Errorf("test case %q: expecting non-empty Smap", tc.name)
				} else if tc.smapVersion != svm.Smap.Version {
					t.Errorf("test case %q: expecting %d, got %d", tc.name, tc.smapVersion, svm.Smap.Version)
				}
			}

			if tc.bmdVersion == 0 {
				if svm.BMD != nil && svm.BMD.version() > 0 {
					t.Errorf("test case %q: expecting nil BMD", tc.name)
				}
			} else {
				if svm.BMD == nil || svm.BMD.version() == 0 {
					t.Errorf("test case %q: expecting non-empty BMD", tc.name)
				} else if tc.bmdVersion != svm.BMD.Version {
					t.Errorf("test case %q: expecting %d, got %d", tc.name, tc.bmdVersion, svm.BMD.Version)
				}
			}
		})
	}
}
