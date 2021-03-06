// Package tutils provides common low-level utilities for all aistore unit and integration tests
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package tutils

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/containers"
	"github.com/NVIDIA/aistore/devtools"
	"github.com/NVIDIA/aistore/devtools/tutils/tassert"
)

const (
	nodeRetryInterval = 2 * time.Second // interval to check for node health
	maxNodeRetry      = 10              // max retries to get health
)

type nodesCnt int

func (n nodesCnt) satisfied(actual int) bool {
	if n == 0 {
		return true
	}
	return int(n) == actual
}

func JoinCluster(proxyURL string, node *cluster.Snode) (string, error) {
	return devtools.JoinCluster(devtoolsCtx, proxyURL, node, registerTimeout)
}

// TODO: There is duplication between `UnregisterNode` and `RemoveTarget` - when to use which?
func RemoveTarget(t *testing.T, proxyURL string, smap *cluster.Smap) (*cluster.Smap, *cluster.Snode) {
	var (
		removeTarget, _ = smap.GetRandTarget()
		origTgtCnt      = smap.CountActiveTargets()
		args            = &cmn.ActValDecommision{DaemonID: removeTarget.ID(), SkipRebalance: true}
	)
	Logf("Removing target %s from %s\n", removeTarget.ID(), smap)

	err := UnregisterNode(proxyURL, args)
	tassert.CheckFatal(t, err)
	newSmap, err := WaitForClusterState(
		proxyURL,
		"target is gone",
		smap.Version,
		smap.CountActiveProxies(),
		origTgtCnt-1,
	)
	tassert.CheckFatal(t, err)
	newTgtCnt := newSmap.CountActiveTargets()
	tassert.Fatalf(t, newTgtCnt == origTgtCnt-1,
		"new smap expected to have 1 target less: %d (v%d) vs %d (v%d)", newTgtCnt, origTgtCnt,
		newSmap.Version, smap.Version)

	return newSmap, removeTarget
}

// TODO: There is duplication between `JoinCluster` and `RestoreTarget` - when to use which?
func RestoreTarget(t *testing.T, proxyURL string, target *cluster.Snode) (rebID string, newSmap *cluster.Smap) {
	smap := GetClusterMap(t, proxyURL)
	tassert.Fatalf(t, smap.GetTarget(target.DaemonID) == nil, "unexpected target %s in smap", target.ID())
	Logf("Reregistering target %s, current Smap: %s\n", target, smap.StringEx())
	rebID, err := JoinCluster(proxyURL, target)
	tassert.CheckFatal(t, err)
	newSmap, err = WaitForClusterState(
		proxyURL,
		"to join target back",
		smap.Version,
		smap.CountActiveProxies(),
		smap.CountActiveTargets()+1,
	)
	tassert.CheckFatal(t, err)
	return rebID, newSmap
}

func ClearMaintenance(baseParams api.BaseParams, tsi *cluster.Snode) {
	val := &cmn.ActValDecommision{DaemonID: tsi.ID(), SkipRebalance: true}
	// it can fail if the node is not under maintenance but it is OK
	_, _ = api.StopMaintenance(baseParams, val)
}

func RandomProxyURL(ts ...*testing.T) (url string) {
	var (
		baseParams = BaseAPIParams(proxyURLReadOnly)
		smap, err  = waitForStartup(baseParams)
		retries    = 3
	)
	if err == nil {
		return getRandomProxyURL(smap)
	}
	for _, node := range pmapReadOnly {
		url := node.URL(cmn.NetworkPublic)
		if url == proxyURLReadOnly {
			continue
		}
		if retries == 0 {
			return ""
		}
		baseParams = BaseAPIParams(url)
		if smap, err = waitForStartup(baseParams); err == nil {
			return getRandomProxyURL(smap)
		}
		retries--
	}
	if len(ts) > 0 {
		tassert.CheckFatal(ts[0], err)
	}

	return ""
}

func getRandomProxyURL(smap *cluster.Smap) string {
	proxies := smap.Pmap.ActiveNodes()
	return proxies[rand.Intn(len(proxies))].URL(cmn.NetworkPublic)
}

// Return the first proxy from smap that is IC member. The primary
// proxy has higher priority.
func GetICProxy(t testing.TB, smap *cluster.Smap, ignoreID string) *cluster.Snode {
	if smap.IsIC(smap.Primary) {
		return smap.Primary
	}
	for _, proxy := range smap.Pmap {
		if ignoreID != "" && proxy.ID() == ignoreID {
			continue
		}
		if !smap.IsIC(proxy) {
			continue
		}
		return proxy
	}
	t.Fatal("failed to choose random IC member")
	return nil
}

// WaitForClusterState waits until a cluster reaches specified state, meaning:
// - smap has version larger than origVersion
// - number of proxies is equal proxyCnt, unless proxyCnt == 0
// - number of targets is equal targetCnt, unless targetCnt == 0.
//
// It returns the smap which satisfies those requirements.
// NOTE: Upon successful return from this function cluster state might have already changed.
func WaitForClusterState(proxyURL, reason string, origVersion int64, proxyCnt, targetCnt int) (*cluster.Smap, error) {
	var (
		lastVersion                               int64
		smapChangeDeadline, timeStart, opDeadline time.Time

		expPrx = nodesCnt(proxyCnt)
		expTgt = nodesCnt(targetCnt)
	)

	timeStart = time.Now()
	smapChangeDeadline = timeStart.Add(2 * proxyChangeLatency)
	opDeadline = timeStart.Add(3 * proxyChangeLatency)

	Logf("Waiting for (p%d, t%d, version > v%d) %s\n", expPrx, expTgt, origVersion, reason)

	var (
		loopCnt    int
		satisfied  bool
		baseParams = BaseAPIParams(proxyURL)
	)

	// Repeat until success or timeout.
	for {
		smap, err := api.GetClusterMap(baseParams)
		if err != nil {
			if !cmn.IsErrConnectionRefused(err) {
				return nil, err
			}
			Logf("%v\n", err)
			goto next
		}

		satisfied = expTgt.satisfied(smap.CountActiveTargets()) &&
			expPrx.satisfied(smap.CountActiveProxies()) &&
			smap.Version > origVersion
		if !satisfied {
			d := time.Since(timeStart)
			Logf("Still polling %s, %s(pid=%s) (%s)\n",
				proxyURL, smap, smap.Primary.ID(), d.Truncate(time.Second))
		}

		if smap.Version != lastVersion {
			smapChangeDeadline = cmn.MinTime(time.Now().Add(proxyChangeLatency), opDeadline)
		}

		// if the primary's map changed to the state we want, wait for the map get populated
		if satisfied {
			syncedSmap := &cluster.Smap{}
			cmn.CopyStruct(syncedSmap, smap)

			// skip primary proxy and mock targets
			var proxyID string
			for _, p := range smap.Pmap {
				if p.PublicNet.DirectURL == proxyURL {
					proxyID = p.ID()
				}
			}
			err = WaitMapVersionSync(smapChangeDeadline, syncedSmap, origVersion, cmn.NewStringSet(MockDaemonID, proxyID))
			if err != nil {
				return nil, err
			}

			if syncedSmap.Version != smap.Version {
				if !expTgt.satisfied(smap.CountActiveTargets()) ||
					!expPrx.satisfied(smap.CountActiveProxies()) {
					return nil, fmt.Errorf("%s changed after sync (to %s) and does not satisfy the state",
						smap, syncedSmap)
				}
				Logf("%s changed after sync (to %s) but satisfies the state\n", smap, syncedSmap)
			}

			return smap, nil
		}

		lastVersion = smap.Version
		loopCnt++
	next:
		if time.Now().After(smapChangeDeadline) {
			break
		}
		// sleep longer each iter (up to a certain limit)
		time.Sleep(cmn.MinDuration(time.Second*time.Duration(loopCnt), time.Second*7))
	}

	return nil, fmt.Errorf("timed out waiting for the cluster to stabilize")
}

func WaitForNewSmap(proxyURL string, prevVersion int64) (newSmap *cluster.Smap, err error) {
	return WaitForClusterState(proxyURL, "new smap version", prevVersion, 0, 0)
}

func WaitMapVersionSync(timeout time.Time, smap *cluster.Smap, prevVersion int64, idsToIgnore cmn.StringSet) error {
	return devtools.WaitMapVersionSync(devtoolsCtx, timeout, smap, prevVersion, idsToIgnore)
}

func GetTargetsMountpaths(t *testing.T, smap *cluster.Smap, params api.BaseParams) map[string][]string {
	mpathsByTarget := make(map[string][]string, smap.CountTargets())
	for _, target := range smap.Tmap {
		mpl, err := api.GetMountpaths(params, target)
		tassert.CheckError(t, err)
		mpathsByTarget[target.DaemonID] = mpl.Available
	}

	return mpathsByTarget
}

func KillNode(node *cluster.Snode) (cmd RestoreCmd, err error) {
	restoreNodesOnce.Do(func() {
		initNodeCmd()
	})

	var (
		daemonID = node.ID()
		port     = node.PublicNet.DaemonPort
		pid      string
	)
	cmd.Node = node
	if containers.DockerRunning() {
		Logf("Stopping container %s\n", daemonID)
		err := containers.StopContainer(daemonID)
		return cmd, err
	}

	pid, cmd.Cmd, cmd.Args, err = getProcess(port)
	if err != nil {
		return
	}
	_, err = exec.Command("kill", "-2", pid).CombinedOutput()
	if err != nil {
		return
	}
	// wait for the process to actually disappear
	to := time.Now().Add(time.Second * 30)
	for {
		_, _, _, errpid := getProcess(port)
		if errpid != nil {
			break
		}
		if time.Now().After(to) {
			err = fmt.Errorf("failed to kill -2 process pid=%s at port %s", pid, port)
			break
		}
		time.Sleep(time.Second)
	}

	exec.Command("kill", "-9", pid).CombinedOutput()
	time.Sleep(time.Second)

	if err != nil {
		_, _, _, errpid := getProcess(port)
		if errpid != nil {
			err = nil
		} else {
			err = fmt.Errorf("failed to kill -9 process pid=%s at port %s", pid, port)
		}
	}
	return
}

func RestoreNode(cmd RestoreCmd, asPrimary bool, tag string) error {
	if containers.DockerRunning() {
		Logf("Restarting %s container %s\n", tag, cmd)
		return containers.RestartContainer(cmd.Node.ID())
	}

	if !cmn.AnyHasPrefixInSlice("-daemon_id", cmd.Args) {
		cmd.Args = append(cmd.Args, "-daemon_id="+cmd.Node.ID())
	}

	Logf("Restoring %s: %s %+v\n", tag, cmd.Cmd, cmd.Args)
	_, err := startNode(cmd.Cmd, cmd.Args, asPrimary)
	return err
}

func startNode(cmd string, args []string, asPrimary bool) (pid int, err error) {
	ncmd := exec.Command(cmd, args...)
	// When using Ctrl-C on test, children (restored daemons) should not be
	// killed as well.
	// (see: https://groups.google.com/forum/#!topic/golang-nuts/shST-SDqIp4)
	ncmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	if asPrimary {
		// Sets the environment variable to start as primary proxy to true
		env := os.Environ()
		env = append(env, fmt.Sprintf("%s=true", cmn.EnvVars.IsPrimary))
		ncmd.Env = env
	}

	if err = ncmd.Start(); err != nil {
		return
	}
	pid = ncmd.Process.Pid
	err = ncmd.Process.Release()
	return
}

func DeployNode(t *testing.T, daeType, cfgPath, daeID string) (int, error) {
	args := []string{
		"-config=" + cfgPath,
		"-daemon_id=" + daeID,
		"-role=" + daeType,
	}

	cmd := getAISNodeCmd(t)
	return startNode(cmd, args, false)
}

// CleanupNode, cleanup the process and directories associated with node
func CleanupNode(t *testing.T, pid int, cfg *cmn.Config, daeTy string) {
	// Make sure the process is killed
	exec.Command("kill", "-9", strconv.Itoa(pid)).CombinedOutput()

	if err := os.RemoveAll(cfg.Confdir); err != nil && !os.IsNotExist(err) {
		t.Error(err.Error())
	}

	if err := os.RemoveAll(cfg.Log.Dir); err != nil && !os.IsNotExist(err) {
		t.Error(err.Error())
	}

	fsWalkFunc := func(p string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			return nil
		}

		if !strings.HasPrefix(info.Name(), "mp") {
			return nil
		}

		deletePath := path.Join(p, info.Name(), strconv.Itoa(cfg.TestFSP.Instance))
		if err := os.RemoveAll(deletePath); err != nil && !os.IsNotExist(err) {
			t.Error(err.Error())
		}
		return nil
	}
	// Clean mountpaths for targets
	if daeTy == cmn.Target {
		filepath.Walk(cfg.TestFSP.Root, fsWalkFunc)
	}
}

// getAISNodeCmd finds the command for deploying AIS node
func getAISNodeCmd(t *testing.T) string {
	// Get command from cached restore CMDs when available
	if len(restoreNodes) != 0 {
		for _, cmd := range restoreNodes {
			return cmd.Cmd
		}
	}

	// If no cached comand, use a random proxy to get command
	proxyURL := RandomProxyURL()
	proxy, err := GetPrimaryProxy(proxyURL)
	tassert.CheckFatal(t, err)
	rcmd := getRestoreCmd(proxy)
	return rcmd.Cmd
}

// getPID uses 'lsof' to find the pid of the ais process listening on a port
func getPID(port string) (string, error) {
	output, err := exec.Command("lsof", []string{"-sTCP:LISTEN", "-i", ":" + port}...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("error executing LSOF command: %v", err)
	}

	// Skip lines before first appearance of "COMMAND"
	lines := strings.Split(string(output), "\n")
	i := 0
	for ; ; i++ {
		if strings.HasPrefix(lines[i], "COMMAND") {
			break
		}
	}

	// second colume is the pid
	return strings.Fields(lines[i+1])[1], nil
}

// getProcess finds the ais process by 'lsof' using a port number, it finds the ais process's
// original command line by 'ps', returns the command line for later to restart(restore) the process.
func getProcess(port string) (string, string, []string, error) {
	pid, err := getPID(port)
	if err != nil {
		return "", "", nil, fmt.Errorf("error getting pid on port: %v", err)
	}

	output, err := exec.Command("ps", "-p", pid, "-o", "command").CombinedOutput()
	if err != nil {
		return "", "", nil, fmt.Errorf("error executing PS command: %v", err)
	}

	line := strings.Split(string(output), "\n")[1]
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", "", nil, fmt.Errorf("no returned fields")
	}

	return pid, fields[0], fields[1:], nil
}

func WaitForNodeToTerminate(pid int, timeout ...time.Duration) error {
	var (
		ctx           = context.Background()
		retryInterval = time.Second
		deadline      = time.Minute
	)

	if len(timeout) > 0 {
		deadline = timeout[0]
	}

	Logf("Waiting for process ID %d to terminate", pid)
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, deadline)
	defer cancel()
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}

	done := make(chan error)
	go func() {
		_, err := process.Wait()
		done <- err
	}()
	for {
		time.Sleep(retryInterval)
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			break
		}
	}
}

func getRestoreCmd(si *cluster.Snode) RestoreCmd {
	var (
		err error
		cmd = RestoreCmd{Node: si}
	)
	if containers.DockerRunning() {
		return cmd
	}
	_, cmd.Cmd, cmd.Args, err = getProcess(si.PublicNet.DaemonPort)
	cmn.AssertNoErr(err)
	return cmd
}

// EnsureOrigClusterState verifies the cluster has the same nodes after tests
// If a node is killed, it restores the node
func EnsureOrigClusterState(t *testing.T) {
	if len(restoreNodes) == 0 {
		return
	}
	var (
		proxyURL       = RandomProxyURL()
		smap           = GetClusterMap(t, proxyURL)
		baseParam      = BaseAPIParams(proxyURL)
		afterProxyCnt  = smap.CountActiveProxies()
		afterTargetCnt = smap.CountActiveTargets()
		tgtCnt         int
		proxyCnt       int
		updated        bool
	)
	for _, cmd := range restoreNodes {
		if cmd.Node.IsProxy() {
			proxyCnt++
		} else {
			tgtCnt++
		}
		node := smap.GetNode(cmd.Node.ID())
		tassert.Errorf(t, node != nil, "%s %s changed its ID", cmd.Node.Type(), cmd.Node.ID())
		if node != nil {
			tassert.Errorf(t, node.Equals(cmd.Node), "%s %s changed, before = %+v, after = %+v", cmd.Node.Type(), node.ID(), cmd.Node, node)
		}

		if containers.DockerRunning() {
			if node == nil {
				RestoreNode(cmd, false, cmd.Node.Type())
				updated = true
			}
			continue
		}

		_, err := getPID(cmd.Node.PublicNet.DaemonPort)
		if err != nil {
			tassert.CheckError(t, err)
			if err = RestoreNode(cmd, false, cmd.Node.Type()); err == nil {
				_, err := WaitNodeAdded(baseParam, cmd.Node.ID())
				tassert.CheckError(t, err)
			}
			tassert.CheckError(t, err)
			updated = true
		}
	}

	tassert.Errorf(
		t, afterProxyCnt == proxyCnt,
		"Some proxies crashed: expected %d, found %d containers",
		proxyCnt, afterProxyCnt,
	)

	tassert.Errorf(
		t, tgtCnt == afterTargetCnt,
		"Some targets crashed: expected %d, found %d containers",
		tgtCnt, afterTargetCnt,
	)

	if !updated {
		return
	}

	_, err := WaitForClusterState(proxyURL, "cluster stabilize", smap.Version, proxyCnt, tgtCnt)
	tassert.CheckFatal(t, err)

	if tgtCnt != afterTargetCnt {
		WaitForRebalanceToComplete(t, BaseAPIParams(proxyURL))
	}
}

func WaitNodeAdded(baseParams api.BaseParams, nodeID string) (*cluster.Smap, error) {
	i := 0

retry:
	smap, err := api.GetClusterMap(baseParams)
	if err != nil {
		return nil, err
	}
	node := smap.GetNode(nodeID)
	if node != nil {
		return smap, WaitNodeReady(node.URL(cmn.NetworkPublic))
	}
	time.Sleep(nodeRetryInterval)
	i++
	if i > maxNodeRetry {
		return nil, fmt.Errorf("max retry (%d) exceeded - node not in smap", maxNodeRetry)
	}

	goto retry
}

func WaitNodeReady(url string) (err error) {
	var (
		i          = 0
		baseParams = BaseAPIParams(url)
	)
while503:
	err = api.Health(baseParams)
	if err == nil {
		return nil
	}
	if !cmn.IsStatusServiceUnavailable(err) && !cmn.IsErrConnectionRefused(err) {
		return
	}
	time.Sleep(nodeRetryInterval)
	i++
	if i > maxNodeRetry {
		return fmt.Errorf("node start failed - max retries (%d) exceeded", maxNodeRetry)
	}
	goto while503
}
