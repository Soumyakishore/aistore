// Package etl provides utilities to initialize and use transformation pods.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package etl

import (
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
	corev1 "k8s.io/api/core/v1"
)

type (
	// Communicator is responsible for managing communications with local ETL container.
	// Do() gets executed as part of (each) GET bucket/object by the user.
	// Communicator listens to cluster membership changes and terminates ETL container,
	// if need be.
	Communicator interface {
		cluster.Slistener

		Name() string
		PodName() string
		SvcName() string
		ConfigMapName() string

		RemoteAddrIP() string

		// Do() uses one of the two ETL container endpoints:
		// - Method "PUT", Path "/"
		// - Method "GET", Path "/bucket/object"
		Do(w http.ResponseWriter, r *http.Request, bck *cluster.Bck, objName string) error

		// Get should be called when there is no incoming request from a user,
		// so there's nothing to redirect/reverse proxy. This is the case for
		// offline-ETL: target starts transforming objects on their own.
		Get(bck *cluster.Bck, objName string) (io.ReadCloser, int64, error)
	}

	commArgs struct {
		listener       cluster.Slistener
		t              cluster.Target
		pod            *corev1.Pod
		commType       string
		podIP          string
		transformerURL string
		name           string
		configMapName  string
	}

	baseComm struct {
		cluster.Slistener
		t cluster.Target

		name          string
		podName       string
		configMapName string

		remoteAddr         string
		transformerAddress string
	}

	pushComm struct {
		baseComm
	}
	redirectComm struct {
		baseComm
	}
	revProxyComm struct {
		baseComm
		rp *httputil.ReverseProxy
	}
)

// interface guard
var (
	_ Communicator = &pushComm{}
	_ Communicator = &redirectComm{}
	_ Communicator = &revProxyComm{}
)

//////////////
// baseComm //
//////////////

func makeCommunicator(args commArgs) Communicator {
	baseComm := baseComm{
		Slistener:          args.listener,
		t:                  args.t,
		name:               args.name,
		podName:            args.pod.GetName(),
		configMapName:      args.configMapName,
		remoteAddr:         args.podIP,
		transformerAddress: args.transformerURL,
	}

	switch args.commType {
	case PushCommType:
		return &pushComm{baseComm: baseComm}
	case RedirectCommType:
		return &redirectComm{baseComm: baseComm}
	case RevProxyCommType:
		transURL, err := url.Parse(args.transformerURL)
		cmn.AssertNoErr(err)
		rp := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				// Replacing the `req.URL` host with ETL container host
				req.URL.Scheme = transURL.Scheme
				req.URL.Host = transURL.Host
				req.URL.RawQuery = pruneQuery(req.URL.RawQuery)
				if _, ok := req.Header["User-Agent"]; !ok {
					// Explicitly disable `User-Agent` so it's not set to default value.
					req.Header.Set("User-Agent", "")
				}
			},
		}
		return &revProxyComm{baseComm: baseComm, rp: rp}
	default:
		cmn.AssertMsg(false, args.commType)
	}
	return nil
}

func (c baseComm) Name() string          { return c.name }
func (c baseComm) PodName() string       { return c.podName }
func (c baseComm) SvcName() string       { return c.podName /*pod name is same as service name*/ }
func (c baseComm) ConfigMapName() string { return c.configMapName }
func (c baseComm) RemoteAddrIP() string  { return c.remoteAddr }

//////////////
// pushComm //
//////////////

func (pushc *pushComm) doRequest(bck *cluster.Bck, objName string) (*http.Response, error) {
	lom := &cluster.LOM{T: pushc.t, ObjName: objName}
	if err := lom.Init(bck.Bck); err != nil {
		return nil, err
	}
	lom.Lock(false)
	defer lom.Unlock(false)
	if err := lom.Load(); err != nil {
		return nil, err
	}

	// `fh` is closed by Do(req)
	fh, err := cmn.NewFileHandle(lom.GetFQN())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPut, pushc.transformerAddress, fh)
	if err != nil {
		return nil, err
	}

	req.ContentLength = lom.Size()
	req.Header.Set(cmn.HeaderContentType, cmn.ContentBinary)
	return pushc.t.Client().Do(req)
}

func (pushc *pushComm) Do(w http.ResponseWriter, _ *http.Request, bck *cluster.Bck, objName string) error {
	resp, err := pushc.doRequest(bck, objName)
	if err != nil {
		return err
	}
	if contentLength := resp.Header.Get(cmn.HeaderContentLength); contentLength != "" {
		w.Header().Add(cmn.HeaderContentLength, contentLength)
	}
	_, err = io.Copy(w, resp.Body)
	debug.AssertNoErr(err)
	err = resp.Body.Close()
	debug.AssertNoErr(err)
	return nil
}

func (pushc *pushComm) Get(bck *cluster.Bck, objName string) (io.ReadCloser, int64, error) {
	resp, err := pushc.doRequest(bck, objName)
	return handleResp(resp, err)
}

////////////////////
//  redirectComm  //
////////////////////

func (repc *redirectComm) Do(w http.ResponseWriter, r *http.Request, bck *cluster.Bck, objName string) error {
	redirectURL := cmn.JoinPath(repc.transformerAddress, transformerPath(bck, objName))
	http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
	return nil
}

func (repc *redirectComm) Get(bck *cluster.Bck, objName string) (io.ReadCloser, int64, error) {
	etlURL := cmn.JoinPath(repc.transformerAddress, transformerPath(bck, objName))
	resp, err := repc.t.Client().Get(etlURL)
	return handleResp(resp, err)
}

//////////////////
// revProxyComm //
//////////////////

func (ppc *revProxyComm) Do(w http.ResponseWriter, r *http.Request, bck *cluster.Bck, objName string) error {
	r.URL.Path = transformerPath(bck, objName) // Reverse proxy should always use /bucket/object endpoint.
	ppc.rp.ServeHTTP(w, r)
	return nil
}

func (ppc *revProxyComm) Get(bck *cluster.Bck, objName string) (io.ReadCloser, int64, error) {
	etlURL := cmn.JoinPath(ppc.transformerAddress, transformerPath(bck, objName))
	resp, err := ppc.t.Client().Get(etlURL)
	return handleResp(resp, err)
}

// prune query (received from AIS proxy) prior to reverse-proxying the request to/from container -
// not removing cmn.URLParamUUID, for instance, would cause infinite loop.
func pruneQuery(rawQuery string) string {
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		glog.Errorf("error parsing raw query: %q", rawQuery)
		return ""
	}
	for _, filtered := range []string{cmn.URLParamUUID, cmn.URLParamProxyID, cmn.URLParamUnixTime} {
		vals.Del(filtered)
	}
	return vals.Encode()
}

func transformerPath(bck *cluster.Bck, objName string) string { return cmn.URLPath(bck.Name, objName) }

func handleResp(resp *http.Response, err error) (io.ReadCloser, int64, error) {
	if err != nil {
		return nil, 0, err
	}

	return resp.Body, resp.ContentLength, nil
}