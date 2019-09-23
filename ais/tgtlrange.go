// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/objwalk"
	"github.com/NVIDIA/aistore/stats"
)

const (
	prefetchChanSize = 200
	defaultDeadline  = 0
	defaultWait      = false

	//list range message keys
	rangePrefix = "prefix"
	rangeRegex  = "regex"
	rangeKey    = "range"
)

type filesWithDeadline struct {
	ctx      context.Context
	objnames []string
	bucket   string
	provider string
	deadline time.Time
	done     chan struct{}
}

type listf func(ct context.Context, objects []string, bucket, provider string, deadline time.Duration, done chan struct{}) error

func (t *targetrunner) getOpFromActionMsg(action string) listf {
	switch action {
	case cmn.ActPrefetch:
		return t.addPrefetchList
	case cmn.ActEvictObjects:
		return t.doListEvict
	case cmn.ActDelete:
		return t.doListDelete
	default:
		return nil
	}
}

//======================
//
// Regex Matching Method
//
//======================

func acceptRegexRange(name, prefix string, regex *regexp.Regexp, min, max int64) bool {
	oname := strings.TrimPrefix(name, prefix)
	s := regex.FindStringSubmatch(oname)
	if s == nil {
		return false
	}
	// If the regex matches:
	if i, err := strconv.ParseInt(s[0], 10, 64); err != nil && s[0] != "" {
		// If the regex matched a non-empty non-number
		return false
	} else if s[0] == "" || ((min == 0 || i >= min) && (max == 0 || i <= max)) {
		// Either the match is empty, or the match is a number.
		// If the match is a number, either min=0 (unset) or it must be above the minimum, and
		// either max=0 (unset) or ir must be below the maximum
		return true
	}
	return false
}

//=============
//
// Delete/Evict
//
//=============

func (t *targetrunner) doListEvictDelete(ct context.Context, evict bool, objs []string,
	bucket, provider string, deadline time.Duration, done chan struct{}) error {
	xdel := t.xactions.renewEvictDelete(evict)
	defer func() {
		if done != nil {
			done <- struct{}{}
		}
		xdel.EndTime(time.Now())
	}()

	var absdeadline time.Time
	if deadline != 0 {
		// 0 is no deadline - if deadline == 0, the absolute deadline is 0 time.
		absdeadline = time.Now().Add(deadline)
	}

	for _, objname := range objs {
		if xdel.Aborted() {
			return nil
		}
		// skip when deadline has expired
		if !absdeadline.IsZero() && time.Now().After(absdeadline) {
			continue
		}
		lom := &cluster.LOM{T: t, Objname: objname}
		err := lom.Init(bucket, provider)
		if err != nil {
			glog.Error(err)
			continue
		}
		err = t.objDelete(ct, lom, evict)
		if err != nil {
			return err
		}

		if lom.Exists() && evict {
			xdel.ObjectsInc()
			xdel.BytesAdd(lom.Size())
		}
	}

	return nil
}

func (t *targetrunner) doListDelete(ct context.Context, objs []string, bucket, provider string,
	deadline time.Duration, done chan struct{}) error {
	return t.doListEvictDelete(ct, false /* evict */, objs, bucket, provider, deadline, done)
}

func (t *targetrunner) doListEvict(ct context.Context, objs []string, bucket, provider string,
	deadline time.Duration, done chan struct{}) error {
	return t.doListEvictDelete(ct, true /* evict */, objs, bucket, provider, deadline, done)
}

//=========
//
// Prefetch
//
//=========

func (t *targetrunner) prefetchMissing(ctx context.Context, objName, bucket, provider string) {
	var (
		vchanged, coldGet bool
	)
	lom := &cluster.LOM{T: t, Objname: objName}
	err := lom.Init(bucket, provider)
	if err != nil {
		glog.Error(err)
		return
	}
	if err = lom.Load(); err != nil {
		glog.Error(err)
		return
	}
	coldGet = !lom.Exists()
	if lom.IsAIS() { // must not come here
		if coldGet {
			glog.Errorf("prefetch: %s", lom)
		}
		return
	}
	if !coldGet && lom.Version() != "" && lom.VerConf().ValidateWarmGet {
		if coldGet, err, _ = t.checkCloudVersion(ctx, lom); err != nil {
			return
		}
	}
	if !coldGet {
		return
	}
	if err, _ = t.GetCold(ctx, lom, true); err != nil {
		if _, ok := err.(*cmn.SkipError); !ok {
			glog.Error(err)
		}
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("prefetch: %s", lom)
	}
	t.statsif.Add(stats.PrefetchCount, 1)
	t.statsif.Add(stats.PrefetchSize, lom.Size())

	if vchanged {
		t.statsif.Add(stats.VerChangeSize, lom.Size())
		t.statsif.Add(stats.VerChangeCount, 1)
	}
}

func (t *targetrunner) addPrefetchList(ct context.Context, objs []string, bucket string, provider string,
	deadline time.Duration, done chan struct{}) error {
	//Validation is checked in target.go
	var absdeadline time.Time
	if deadline != 0 {
		// 0 is no deadline - if deadline == 0, the absolute deadline is 0 time.
		absdeadline = time.Now().Add(deadline)
	}
	t.prefetchQueue <- filesWithDeadline{ctx: ct, objnames: objs, bucket: bucket, provider: provider, deadline: absdeadline, done: done}
	return nil
}

//================
//
// Message Parsing
//
//================

func unmarshalMsgValue(jsmap map[string]interface{}, key string) (val string, err error) {
	v, ok := jsmap[key]
	if !ok {
		err = fmt.Errorf("no %s field in map", key)
		return
	}
	if val, ok = v.(string); !ok {
		err = fmt.Errorf("value ((%+v, %T) corresponding to key (%s) in map is not of string type", v, v, key)
	}
	return
}

func parseBaseMsg(jsmap map[string]interface{}) (pbm *cmn.ListRangeMsgBase, err error) {
	var (
		deadline time.Duration
	)
	pbm = &cmn.ListRangeMsgBase{Deadline: defaultDeadline, Wait: defaultWait}
	if v, ok := jsmap["deadline"]; ok {
		// When unmarshalling map[string]interface{},
		// Go will convert int to float64 (https://play.golang.org/p/kHroZ1rHVQ)
		if f, ok := v.(float64); ok {
			deadline = time.Duration(int64(f))
		} else {
			deadline, err = time.ParseDuration(v.(string))
		}

		if err != nil {
			return pbm, fmt.Errorf("error parsing BaseMsg: (Deadline: %v, %T, %v)", v, v, err)
		}
		pbm.Deadline = deadline
	}
	if v, ok := jsmap["wait"]; ok {
		wait, ok := v.(bool)
		if !ok {
			return pbm, fmt.Errorf("error parsing BaseMsg: (Wait: %v, %T)", v, v)
		}
		pbm.Wait = wait
	}
	return
}

func parseListMsg(jsmap map[string]interface{}) (pm *cmn.ListMsg, err error) {
	pbm, err := parseBaseMsg(jsmap)
	if err != nil {
		return
	}
	pm = &cmn.ListMsg{ListRangeMsgBase: *pbm}
	v, ok := jsmap["objnames"]
	if !ok {
		return pm, errors.New("error parsing ListMsg: no 'objnames' field")
	}
	if objNames, ok := v.([]interface{}); ok {
		pm.Objnames = make([]string, 0, len(objNames))
		for _, obj := range objNames {
			objName, ok := obj.(string)
			if !ok {
				return pm, errors.New("error parsing ListMsg: non-string 'objnames' field")
			}
			pm.Objnames = append(pm.Objnames, objName)
		}
	} else {
		return pm, errors.New("error parsing ListMsg: couldn't parse 'objnames' field")
	}
	return
}

func parseRangeMsg(jsmap map[string]interface{}) (pm *cmn.RangeMsg, err error) {
	pbm, err := parseBaseMsg(jsmap)
	if err != nil {
		return
	}
	pm = &cmn.RangeMsg{ListRangeMsgBase: *pbm}

	prefix, err := unmarshalMsgValue(jsmap, rangePrefix)
	if err != nil {
		return pm, fmt.Errorf("error parsing RangeMsg: %s", err)
	}
	pm.Prefix = prefix

	regex, err := unmarshalMsgValue(jsmap, rangeRegex)
	if err != nil {
		return pm, fmt.Errorf("error parsing RangeMsg: %s", err)
	}
	pm.Regex = regex

	r, err := unmarshalMsgValue(jsmap, rangeKey)
	if err != nil {
		return pm, fmt.Errorf("error parsing RangeMsg: %s", err)
	}
	pm.Range = r

	return
}

func parseRange(rangestr string) (min, max int64, err error) {
	if rangestr != "" {
		ranges := strings.Split(rangestr, ":")
		if ranges[0] == "" {
			// Min was not set
			min = 0
		} else {
			min, err = strconv.ParseInt(ranges[0], 10, 64)
			if err != nil {
				return
			}
		}

		if ranges[1] == "" {
			// Max was not set
			max = 0
		} else {
			max, err = strconv.ParseInt(ranges[1], 10, 64)
			if err != nil {
				return
			}
		}
	} else {
		min = 0
		max = 0
	}
	return
}

//=======================================================================
//
// Method called by target to execute 1) prefetch, 2) evict, or 3) delete
//
//=======================================================================

func (t *targetrunner) listRangeOperation(r *http.Request, apitems []string, provider string, msgInt *actionMsgInternal) error {
	operation := t.getOpFromActionMsg(msgInt.Action)
	if operation == nil {
		return fmt.Errorf("invalid operation")
	}

	details := fmt.Sprintf(" (%s, %s, %T)", msgInt.Action, msgInt.Name, msgInt.Value)
	jsmap, ok := msgInt.Value.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid cmn.ActionMsg.Value format %s", details)
	}
	if _, ok := jsmap["objnames"]; !ok {
		// Parse map into RangeMsg, convert to and process ListMsg page-by-page
		rangeMsg, err := parseRangeMsg(jsmap)
		if err != nil {
			return fmt.Errorf("%v: %s", err, details)
		}
		return t.iterateBucketListPages(r, apitems, provider, rangeMsg, operation)
	}
	// Parse map into ListMsg
	listMsg, err := parseListMsg(jsmap)
	if err != nil {
		return fmt.Errorf("%v: %s", err, details)
	}
	return t.listOperation(r, apitems, provider, listMsg, operation)
}

func (t *targetrunner) listOperation(r *http.Request, apitems []string, provider string, listMsg *cmn.ListMsg, f listf) error {
	var (
		err    error
		bucket = apitems[0]
		objs   = make([]string, 0, len(listMsg.Objnames))
		smap   = t.smapowner.get()
	)
	for _, obj := range listMsg.Objnames {
		si, err := cluster.HrwTarget(bucket, obj, &smap.Smap)
		if err != nil {
			return err
		}
		if si.DaemonID == t.si.DaemonID {
			objs = append(objs, obj)
		}
	}

	if len(objs) != 0 {
		var (
			done  chan struct{}
			errCh chan error
		)

		if listMsg.Wait {
			done = make(chan struct{}, 1)
			defer close(done)

			errCh = make(chan error)
			defer close(errCh)
		}

		// Asynchronously perform function
		go func() {
			err := f(t.contextWithAuth(r.Header), objs, bucket, provider, listMsg.Deadline, done)
			if err != nil {
				glog.Errorf("Error performing list function: %v", err)
				t.statsif.Add(stats.ErrListCount, 1)
			}
			if errCh != nil {
				errCh <- err
			}
		}()

		if listMsg.Wait {
			<-done
			err = <-errCh
		}
	}
	return err
}

func (t *targetrunner) iterateBucketListPages(r *http.Request, apitems []string, provider string, rangeMsg *cmn.RangeMsg, operation listf) error {
	var (
		bucketListPage *cmn.BucketList
		err            error
		bucket         = apitems[0]
		prefix         = rangeMsg.Prefix
		ctx            = t.contextWithAuth(r.Header)
		msg            = &cmn.SelectMsg{Prefix: prefix, Props: cmn.GetPropsStatus}
	)

	bck := &cluster.Bck{Name: bucket, Provider: provider}
	if err := bck.Init(t.bmdowner); err != nil {
		return err
	}

	min, max, err := parseRange(rangeMsg.Range)
	if err != nil {
		return fmt.Errorf("error parsing range string (%s): %v", rangeMsg.Range, err)
	}

	re, err := regexp.Compile(rangeMsg.Regex)
	if err != nil {
		return fmt.Errorf("could not compile regex: %v", err)
	}

	for {
		if bck.IsAIS() {
			walk := objwalk.NewWalk(context.Background(), t, bck, msg)
			bucketListPage, err = walk.LocalObjPage()
		} else {
			bucketListPage, err, _ = t.Cloud().ListBucket(ctx, bck.Name, msg)
		}
		if err != nil {
			return err
		}
		if len(bucketListPage.Entries) == 0 {
			break
		}

		matchingEntries := make([]string, 0, len(bucketListPage.Entries))
		for _, be := range bucketListPage.Entries {
			if !be.IsStatusOK() {
				continue
			}
			if !acceptRegexRange(be.Name, prefix, re, min, max) {
				continue
			}
			matchingEntries = append(matchingEntries, be.Name)
		}

		if len(matchingEntries) != 0 {
			// Create a ListMsg with a single page of BucketList containing BucketEntries
			listMsg := &cmn.ListMsg{
				ListRangeMsgBase: rangeMsg.ListRangeMsgBase,
				Objnames:         matchingEntries,
			}

			// Call listrange function with paged chunk of entries
			if err := t.listOperation(r, apitems, provider, listMsg, operation); err != nil {
				return err
			}
		}
		// Stop when the last page of BucketList is reached
		if bucketListPage.PageMarker == "" {
			break
		}

		// Update PageMarker for the next request
		msg.PageMarker = bucketListPage.PageMarker
	}
	return nil
}