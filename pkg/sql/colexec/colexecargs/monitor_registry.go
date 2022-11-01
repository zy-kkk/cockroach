// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package colexecargs

import (
	"context"
	"strconv"

	"github.com/cockroachdb/cockroach/pkg/sql/colexecerror"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

// MonitorRegistry instantiates and keeps track of the memory monitoring
// infrastructure in the vectorized engine.
type MonitorRegistry struct {
	accounts []*mon.BoundAccount
	monitors []*mon.BytesMonitor
}

// GetMonitors returns all the monitors from the registry.
func (r *MonitorRegistry) GetMonitors() []*mon.BytesMonitor {
	return r.monitors
}

// NewStreamingMemAccount creates a new memory account bound to the monitor in
// flowCtx.
func (r *MonitorRegistry) NewStreamingMemAccount(flowCtx *execinfra.FlowCtx) *mon.BoundAccount {
	streamingMemAccount := flowCtx.EvalCtx.Mon.MakeBoundAccount()
	r.accounts = append(r.accounts, &streamingMemAccount)
	return &streamingMemAccount
}

// getMemMonitorName returns a unique (for this MonitorRegistry) memory monitor
// name.
func (r *MonitorRegistry) getMemMonitorName(
	opName redact.RedactableString, processorID int32, suffix redact.RedactableString,
) redact.RedactableString {
	return opName + "-" + redact.RedactableString(strconv.Itoa(int(processorID))) + "-" +
		suffix + "-" + redact.RedactableString(strconv.Itoa(len(r.monitors)))
}

// CreateMemAccountForSpillStrategy instantiates a memory monitor and a memory
// account to be used with a buffering colexecop.Operator that can fall back to
// disk. The default memory limit is used, if flowCtx.Cfg.ForceDiskSpill is
// used, this will be 1. The receiver is updated to have references to both
// objects. Memory monitor name is also returned.
func (r *MonitorRegistry) CreateMemAccountForSpillStrategy(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	opName redact.RedactableString,
	processorID int32,
) (*mon.BoundAccount, redact.RedactableString) {
	monitorName := r.getMemMonitorName(opName, processorID, "limited" /* suffix */)
	bufferingOpMemMonitor := execinfra.NewLimitedMonitor(
		ctx, flowCtx.EvalCtx.Mon, flowCtx, monitorName,
	)
	r.monitors = append(r.monitors, bufferingOpMemMonitor)
	bufferingMemAccount := bufferingOpMemMonitor.MakeBoundAccount()
	r.accounts = append(r.accounts, &bufferingMemAccount)
	return &bufferingMemAccount, monitorName
}

// CreateMemAccountForSpillStrategyWithLimit is the same as
// CreateMemAccountForSpillStrategy except that it takes in a custom limit
// instead of using the number obtained via execinfra.GetWorkMemLimit. Memory
// monitor name is also returned.
func (r *MonitorRegistry) CreateMemAccountForSpillStrategyWithLimit(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	limit int64,
	opName redact.RedactableString,
	processorID int32,
) (*mon.BoundAccount, redact.RedactableString) {
	if flowCtx.Cfg.TestingKnobs.ForceDiskSpill {
		if limit != 1 {
			colexecerror.InternalError(errors.AssertionFailedf(
				"expected limit of 1 when forcing disk spilling, got %d", limit,
			))
		}
	}
	monitorName := r.getMemMonitorName(opName, processorID, "limited" /* suffix */)
	bufferingOpMemMonitor := mon.NewMonitorInheritWithLimit(monitorName, limit, flowCtx.EvalCtx.Mon)
	bufferingOpMemMonitor.StartNoReserved(ctx, flowCtx.EvalCtx.Mon)
	r.monitors = append(r.monitors, bufferingOpMemMonitor)
	bufferingMemAccount := bufferingOpMemMonitor.MakeBoundAccount()
	r.accounts = append(r.accounts, &bufferingMemAccount)
	return &bufferingMemAccount, monitorName
}

// CreateExtraMemAccountForSpillStrategy can be used to derive another memory
// account that is bound to the memory monitor specified by the monitorName. It
// is expected that such a monitor with a such name was already created by the
// MonitorRegistry. If no such monitor is found, then nil is returned.
func (r *MonitorRegistry) CreateExtraMemAccountForSpillStrategy(
	monitorName string,
) *mon.BoundAccount {
	// Iterate backwards since most likely that we want to create an account
	// bound to the most recently created monitor.
	for i := len(r.monitors) - 1; i >= 0; i-- {
		if r.monitors[i].Name() == monitorName {
			bufferingMemAccount := r.monitors[i].MakeBoundAccount()
			r.accounts = append(r.accounts, &bufferingMemAccount)
			return &bufferingMemAccount
		}
	}
	return nil
}

// CreateUnlimitedMemAccounts instantiates an unlimited memory monitor (with a
// unique monitor name) and a number of memory accounts bound to it. The
// receiver is updated to have references to all objects. Note that the returned
// accounts are only "unlimited" in that they do not have a hard limit that they
// enforce, but a limit might be enforced by a root monitor.
//
// Note that the memory monitor name is not returned (unlike above) because no
// caller actually needs it.
func (r *MonitorRegistry) CreateUnlimitedMemAccounts(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	opName redact.RedactableString,
	processorID int32,
	numAccounts int,
) []*mon.BoundAccount {
	monitorName := r.getMemMonitorName(opName, processorID, "unlimited" /* suffix */)
	_, accounts := r.createUnlimitedMemAccounts(ctx, flowCtx, monitorName, numAccounts)
	return accounts
}

// CreateUnlimitedMemAccount is a light wrapper around
// CreateUnlimitedMemAccounts when only a single account is desired.
func (r *MonitorRegistry) CreateUnlimitedMemAccount(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	opName redact.RedactableString,
	processorID int32,
) *mon.BoundAccount {
	return r.CreateUnlimitedMemAccounts(ctx, flowCtx, opName, processorID, 1 /* numAccounts */)[0]
}

// CreateUnlimitedMemAccountsWithName is similar to CreateUnlimitedMemAccounts
// with the only difference that the monitor name is provided by the caller.
func (r *MonitorRegistry) CreateUnlimitedMemAccountsWithName(
	ctx context.Context, flowCtx *execinfra.FlowCtx, name redact.RedactableString, numAccounts int,
) (*mon.BytesMonitor, []*mon.BoundAccount) {
	return r.createUnlimitedMemAccounts(ctx, flowCtx, name+"-unlimited", numAccounts)
}

func (r *MonitorRegistry) createUnlimitedMemAccounts(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	monitorName redact.RedactableString,
	numAccounts int,
) (*mon.BytesMonitor, []*mon.BoundAccount) {
	bufferingOpUnlimitedMemMonitor := execinfra.NewMonitor(
		ctx, flowCtx.EvalCtx.Mon, monitorName,
	)
	r.monitors = append(r.monitors, bufferingOpUnlimitedMemMonitor)
	oldLen := len(r.accounts)
	for i := 0; i < numAccounts; i++ {
		acc := bufferingOpUnlimitedMemMonitor.MakeBoundAccount()
		r.accounts = append(r.accounts, &acc)
	}
	return bufferingOpUnlimitedMemMonitor, r.accounts[oldLen:len(r.accounts)]
}

// CreateDiskMonitor instantiates an unlimited disk monitor.
func (r *MonitorRegistry) CreateDiskMonitor(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	opName redact.RedactableString,
	processorID int32,
) *mon.BytesMonitor {
	monitorName := r.getMemMonitorName(opName, processorID, "disk" /* suffix */)
	opDiskMonitor := execinfra.NewMonitor(ctx, flowCtx.DiskMonitor, monitorName)
	r.monitors = append(r.monitors, opDiskMonitor)
	return opDiskMonitor
}

// CreateDiskAccount instantiates an unlimited disk monitor and a disk account
// to be used for disk spilling infrastructure in vectorized engine.
//
// Note that the memory monitor name is not returned (unlike above) because no
// caller actually needs it.
func (r *MonitorRegistry) CreateDiskAccount(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	opName redact.RedactableString,
	processorID int32,
) *mon.BoundAccount {
	opDiskMonitor := r.CreateDiskMonitor(ctx, flowCtx, opName, processorID)
	opDiskAccount := opDiskMonitor.MakeBoundAccount()
	r.accounts = append(r.accounts, &opDiskAccount)
	return &opDiskAccount
}

// CreateDiskAccounts instantiates an unlimited disk monitor and disk accounts
// to be used for disk spilling infrastructure in vectorized engine.
func (r *MonitorRegistry) CreateDiskAccounts(
	ctx context.Context, flowCtx *execinfra.FlowCtx, name redact.RedactableString, numAccounts int,
) (*mon.BytesMonitor, []*mon.BoundAccount) {
	diskMonitor := execinfra.NewMonitor(ctx, flowCtx.DiskMonitor, name)
	r.monitors = append(r.monitors, diskMonitor)
	oldLen := len(r.accounts)
	for i := 0; i < numAccounts; i++ {
		diskAcc := diskMonitor.MakeBoundAccount()
		r.accounts = append(r.accounts, &diskAcc)
	}
	return diskMonitor, r.accounts[oldLen:len(r.accounts)]
}

// AssertInvariants confirms that all invariants are maintained by
// MonitorRegistry.
func (r *MonitorRegistry) AssertInvariants() {
	// Check that all memory monitor names are unique (colexec.diskSpillerBase
	// relies on this in order to catch "memory budget exceeded" errors only
	// from "its own" component).
	names := make(map[string]struct{}, len(r.monitors))
	for _, m := range r.monitors {
		if _, seen := names[m.Name()]; seen {
			colexecerror.InternalError(errors.AssertionFailedf("monitor named %q encountered twice", m.Name()))
		}
		names[m.Name()] = struct{}{}
	}
}

// Close closes all components in the registry.
func (r *MonitorRegistry) Close(ctx context.Context) {
	for i := range r.accounts {
		r.accounts[i].Close(ctx)
	}
	for i := range r.monitors {
		r.monitors[i].Stop(ctx)
	}
}

// Reset prepares the registry for reuse.
func (r *MonitorRegistry) Reset() {
	// There is no need to deeply reset the memory monitoring infra slices
	// because these objects are very tiny in the grand scheme of things.
	r.accounts = r.accounts[:0]
	r.monitors = r.monitors[:0]
}
