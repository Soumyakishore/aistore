// Package etl provides utilities to initialize and use transformation pods.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package etl

import (
	"strings"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/k8s"
	"github.com/NVIDIA/aistore/etl/runtime"
)

func Build(t cluster.Target, msg BuildMsg) error {
	// Initialize runtime.
	r, exists := runtime.Runtimes[msg.Runtime]
	cmn.Assert(exists) // Runtime should be checked in proxy during validation.

	var (
		// We clean up the `msg.ID` as K8s doesn't allow `_` and uppercase
		// letters in the names.
		name    = k8s.CleanName("etl-" + msg.ID)
		podSpec = r.PodSpec()
	)

	podSpec = strings.ReplaceAll(podSpec, "<NAME>", name)

	// Finally, start the ETL with declared Pod specification.
	return Start(t, InitMsg{
		ID:          msg.ID,
		Spec:        []byte(podSpec),
		CommType:    PushCommType,
		WaitTimeout: msg.WaitTimeout,
	}, StartOpts{Env: map[string]string{
		r.CodeEnvName(): string(msg.Code),
		r.DepsEnvName(): string(msg.Deps),
	}})
}
