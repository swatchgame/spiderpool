// Copyright 2022 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0

package ippoolmanager

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/strings/slices"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/spidernet-io/spiderpool/api/v1/agent/models"
	"github.com/spidernet-io/spiderpool/pkg/constant"
	"github.com/spidernet-io/spiderpool/pkg/election"
	spiderpoolip "github.com/spidernet-io/spiderpool/pkg/ip"
	spiderpoolv1 "github.com/spidernet-io/spiderpool/pkg/k8s/apis/v1"
	"github.com/spidernet-io/spiderpool/pkg/logutils"
	"github.com/spidernet-io/spiderpool/pkg/namespacemanager"
	"github.com/spidernet-io/spiderpool/pkg/nodemanager"
	"github.com/spidernet-io/spiderpool/pkg/podmanager"
	"github.com/spidernet-io/spiderpool/pkg/reservedipmanager"
	"github.com/spidernet-io/spiderpool/pkg/types"
)

var logger = logutils.Logger.Named("IPPool-Manager")

type IPPoolManager interface {
	AllocateIP(ctx context.Context, poolName, containerID, nic string, pod *corev1.Pod) (*models.IPConfig, error)
	ReleaseIP(ctx context.Context, poolName string, ipAndCIDs []IPAndCID) error
	ListAllIPPools(ctx context.Context) (*spiderpoolv1.IPPoolList, error)
	SelectByPod(ctx context.Context, version types.IPVersion, poolName string, pod *corev1.Pod) (bool, error)
	CheckVlanSame(ctx context.Context, poolList []string) (map[types.Vlan][]string, bool, error)
	GetIPPoolByName(ctx context.Context, podName string) (*spiderpoolv1.IPPool, error)
	RemoveFinalizer(ctx context.Context, poolName string) error
	AssembleTotalIPs(ctx context.Context, ipPool *spiderpoolv1.IPPool) ([]net.IP, error)
	SetupReconcile(leader election.SpiderLeaseElector) error
	SetupWebhook() error
}

type ipPoolManager struct {
	client                client.Client
	runtimeMgr            ctrl.Manager
	rIPManager            reservedipmanager.ReservedIPManager
	nodeManager           nodemanager.NodeManager
	nsManager             namespacemanager.NamespaceManager
	podManager            podmanager.PodManager
	maxAllocatedIPs       int
	maxConflictRetrys     int
	conflictRetryUnitTime time.Duration

	leader election.SpiderLeaseElector
}

func NewIPPoolManager(mgr ctrl.Manager, rIPManager reservedipmanager.ReservedIPManager, nodeManager nodemanager.NodeManager, nsManager namespacemanager.NamespaceManager, podManager podmanager.PodManager, maxAllocatedIPs, maxConflictRetrys int, conflictRetryUnitTime time.Duration) (IPPoolManager, error) {
	if mgr == nil {
		return nil, errors.New("runtime manager must be specified")
	}
	if rIPManager == nil {
		return nil, errors.New("reserved IP manager must be specified")
	}
	if nodeManager == nil {
		return nil, errors.New("node manager must be specified")
	}
	if nsManager == nil {
		return nil, errors.New("namespace manager must be specified")
	}
	if podManager == nil {
		return nil, errors.New("pod manager must be specified")
	}

	return &ipPoolManager{
		client:                mgr.GetClient(),
		runtimeMgr:            mgr,
		rIPManager:            rIPManager,
		nodeManager:           nodeManager,
		nsManager:             nsManager,
		podManager:            podManager,
		maxAllocatedIPs:       maxAllocatedIPs,
		maxConflictRetrys:     maxConflictRetrys,
		conflictRetryUnitTime: conflictRetryUnitTime,
	}, nil
}

func (r *ipPoolManager) AllocateIP(ctx context.Context, poolName, containerID, nic string, pod *corev1.Pod) (*models.IPConfig, error) {

	// TODO(iiiceoo): STS static ip, check "EnableStatuflsetIP"
	// ownerType := r.podManager.GetOwnerType(ctx, pod)

	var ipConfig *models.IPConfig
	rand.Seed(time.Now().UnixNano())
	for i := 0; i <= r.maxConflictRetrys; i++ {
		var ipPool spiderpoolv1.IPPool
		if err := r.client.Get(ctx, apitypes.NamespacedName{Name: poolName}, &ipPool); err != nil {
			return nil, err
		}

		// TODO(iiiceoo): Check TotalIPCount - AllocatedIPCount

		reserved, err := r.rIPManager.GetReservedIPRanges(ctx, *ipPool.Spec.IPVersion)
		if err != nil {
			return nil, err
		}

		var used []string
		for ip := range ipPool.Status.AllocatedIPs {
			used = append(used, ip)
		}

		// TODO(iiiceoo): refactor
		allocateIP, err := randomIP(*ipPool.Spec.IPVersion, ipPool.Spec.IPs, used, ipPool.Spec.ExcludeIPs, reserved)
		if err != nil {
			return nil, err
		}

		// TODO(iiiceoo): Remove when Defaulter webhook work
		if ipPool.Status.AllocatedIPs == nil {
			ipPool.Status.AllocatedIPs = spiderpoolv1.PoolIPAllocations{}
		}
		ipPool.Status.AllocatedIPs[allocateIP.String()] = spiderpoolv1.PoolIPAllocation{
			ContainerID: containerID,
			NIC:         nic,
			Node:        pod.Spec.NodeName,
			Namespace:   pod.Namespace,
			Pod:         pod.Name,
		}

		// TODO(iiiceoo): Remove when Defaulter webhook work
		if ipPool.Status.AllocatedIPCount == nil {
			ipPool.Status.AllocatedIPCount = new(int64)
		}

		*ipPool.Status.AllocatedIPCount++
		if *ipPool.Status.AllocatedIPCount > int64(r.maxAllocatedIPs) {
			return nil, fmt.Errorf("threshold of IP allocations(<=%d) for IP pool exceeded: %w", r.maxAllocatedIPs, constant.ErrIPUsedOut)
		}

		if err := r.client.Status().Update(ctx, &ipPool); err != nil {
			if apierrors.IsConflict(err) {
				if i == r.maxConflictRetrys {
					return nil, fmt.Errorf("insufficient retries(<=%d) to allocate IP from IP pool %s", r.maxConflictRetrys, poolName)
				}

				time.Sleep(time.Duration(rand.Intn(1<<(i+1))) * r.conflictRetryUnitTime)
				continue
			}
			return nil, err
		}

		ipConfig, err = genResIPConfig(allocateIP, &ipPool.Spec, nic, poolName)
		if err != nil {
			return nil, err
		}
		break
	}

	return ipConfig, nil
}

func randomIP(version types.IPVersion, all []string, used []string, exclude []string, reserved []string) (net.IP, error) {
	reservedIPs, err := spiderpoolip.ParseIPRanges(version, reserved)
	if err != nil {
		return nil, err
	}
	usedIPs, err := spiderpoolip.ParseIPRanges(version, used)
	if err != nil {
		return nil, err
	}
	expectIPs, err := spiderpoolip.ParseIPRanges(version, all)
	if err != nil {
		return nil, err
	}
	excludeIPs, err := spiderpoolip.ParseIPRanges(version, exclude)
	if err != nil {
		return nil, err
	}
	availableIPs := spiderpoolip.IPsDiffSet(expectIPs, append(reservedIPs, append(usedIPs, excludeIPs...)...))

	if len(availableIPs) == 0 {
		return nil, constant.ErrIPUsedOut
	}

	return availableIPs[rand.Int()%len(availableIPs)], nil
}

type IPAndCID struct {
	IP          string
	ContainerID string
}

func (r *ipPoolManager) ReleaseIP(ctx context.Context, poolName string, ipAndCIDs []IPAndCID) error {
	rand.Seed(time.Now().UnixNano())
	for i := 0; i <= r.maxConflictRetrys; i++ {
		var ipPool spiderpoolv1.IPPool
		if err := r.client.Get(ctx, apitypes.NamespacedName{Name: poolName}, &ipPool); err != nil {
			return err
		}

		// TODO(iiiceoo): Remove when Defaulter webhook work
		if ipPool.Status.AllocatedIPs == nil {
			ipPool.Status.AllocatedIPs = spiderpoolv1.PoolIPAllocations{}
		}
		if ipPool.Status.AllocatedIPCount == nil {
			ipPool.Status.AllocatedIPCount = new(int64)
		}

		needRelease := false
		for _, e := range ipAndCIDs {
			if a, ok := ipPool.Status.AllocatedIPs[e.IP]; ok {
				if a.ContainerID == e.ContainerID {
					delete(ipPool.Status.AllocatedIPs, e.IP)
					*ipPool.Status.AllocatedIPCount--
					needRelease = true
				}
			}
		}

		if !needRelease {
			return nil
		}

		if err := r.client.Status().Update(ctx, &ipPool); err != nil {
			if apierrors.IsConflict(err) {
				if i == r.maxConflictRetrys {
					return fmt.Errorf("insufficient retries(<=%d) to release IP %+v from IP pool %s", r.maxConflictRetrys, ipAndCIDs, poolName)
				}

				time.Sleep(time.Duration(rand.Intn(1<<(i+1))) * r.conflictRetryUnitTime)
				continue
			}
			return err
		}
		break
	}

	return nil
}

func (r *ipPoolManager) ListAllIPPools(ctx context.Context) (*spiderpoolv1.IPPoolList, error) {
	ippoolList := &spiderpoolv1.IPPoolList{}
	err := r.client.List(ctx, ippoolList)
	if nil != err {
		return nil, err
	}

	return ippoolList, nil
}

func (r *ipPoolManager) SelectByPod(ctx context.Context, version types.IPVersion, poolName string, pod *corev1.Pod) (bool, error) {
	logger := logutils.FromContext(ctx)

	var ipPool spiderpoolv1.IPPool
	if err := r.client.Get(ctx, apitypes.NamespacedName{Name: poolName}, &ipPool); err != nil {
		logger.Sugar().Warnf("IP pool %s is not found", poolName)
		return false, client.IgnoreNotFound(err)
	}

	if ipPool.DeletionTimestamp != nil {
		logger.Sugar().Warnf("IP pool %s is terminating", poolName)
		return false, nil
	}

	if *ipPool.Spec.Disable {
		logger.Sugar().Warnf("IP pool %s is disable", poolName)
		return false, nil
	}

	if *ipPool.Spec.IPVersion != version {
		logger.Sugar().Warnf("IP pool %s has different version with specified via input", poolName)
		return false, nil
	}

	if ipPool.Spec.NodeSelector != nil {
		nodeMatched, err := r.nodeManager.MatchLabelSelector(ctx, pod.Spec.NodeName, ipPool.Spec.NodeSelector)
		if err != nil {
			return false, err
		}
		if !nodeMatched {
			logger.Sugar().Infof("Unmatched Node selector, IP pool %s is filtered", poolName)
			return false, nil
		}
	}

	if ipPool.Spec.NamesapceSelector != nil {
		nsMatched, err := r.nsManager.MatchLabelSelector(ctx, pod.Namespace, ipPool.Spec.NamesapceSelector)
		if err != nil {
			return false, err
		}
		if !nsMatched {
			logger.Sugar().Infof("Unmatched Namespace selector, IP pool %s is filtered", poolName)
			return false, nil
		}
	}

	if ipPool.Spec.PodSelector != nil {
		podMatched, err := r.podManager.MatchLabelSelector(ctx, pod.Namespace, pod.Name, ipPool.Spec.PodSelector)
		if err != nil {
			return false, err
		}
		if !podMatched {
			logger.Sugar().Infof("Unmatched Pod selector, IP pool %s is filtered", poolName)
			return false, nil
		}
	}

	return true, nil
}

func (r *ipPoolManager) CheckVlanSame(ctx context.Context, poolList []string) (map[types.Vlan][]string, bool, error) {
	vlanToPools := map[types.Vlan][]string{}
	for _, p := range poolList {
		var ipPool spiderpoolv1.IPPool
		if err := r.client.Get(ctx, apitypes.NamespacedName{Name: p}, &ipPool); err != nil {
			return nil, false, err
		}

		vlanToPools[*ipPool.Spec.Vlan] = append(vlanToPools[*ipPool.Spec.Vlan], p)
	}

	if len(vlanToPools) > 1 {
		return vlanToPools, false, nil
	}

	return vlanToPools, true, nil
}

func (r *ipPoolManager) GetIPPoolByName(ctx context.Context, poolName string) (*spiderpoolv1.IPPool, error) {
	var ipPool spiderpoolv1.IPPool
	err := r.client.Get(ctx, apitypes.NamespacedName{Name: poolName}, &ipPool)
	if nil != err {
		return nil, err
	}

	return &ipPool, nil
}

func (r *ipPoolManager) RemoveFinalizer(ctx context.Context, poolName string) error {
	ipPool, err := r.GetIPPoolByName(ctx, poolName)
	if nil != err {
		if apierrors.IsNotFound(err) {
			logger.Sugar().Debugf("IPPool '%s' not found", poolName)
			return nil
		}

		return err
	}

	if slices.Contains(ipPool.Finalizers, constant.SpiderFinalizer) {
		controllerutil.RemoveFinalizer(ipPool, constant.SpiderFinalizer)

		err = r.client.Update(ctx, ipPool)
		if nil != err {
			return err
		}
	}

	return nil
}

// AssembleTotalIP will calculate an IPPool CR object usable IPs number,
// it summaries the IPPool IPs then subtracts ExcludeIPs.
// notice: this method would not filter ReservedIP CR object data!
func (r *ipPoolManager) AssembleTotalIPs(ctx context.Context, ipPool *spiderpoolv1.IPPool) ([]net.IP, error) {
	// TODO (Icarus9913): ips could be nil, should we return error?
	ips, err := spiderpoolip.ParseIPRanges(*ipPool.Spec.IPVersion, ipPool.Spec.IPs)
	if nil != err {
		return nil, err
	}
	excludeIPs, err := spiderpoolip.ParseIPRanges(*ipPool.Spec.IPVersion, ipPool.Spec.ExcludeIPs)
	if nil != err {
		return nil, err
	}
	usableIPs := spiderpoolip.IPsDiffSet(ips, excludeIPs)

	return usableIPs, nil
}
