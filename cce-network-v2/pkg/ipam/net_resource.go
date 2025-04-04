/*
 * Copyright (c) 2023 Baidu, Inc. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
 * except in compliance with the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the
 * License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions
 * and limitations under the License.
 *
 */

package ipam

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/sirupsen/logrus"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"

	operatorOption "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/operator/option"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/operator/watchers"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/defaults"
	ipamOption "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/ipam/option"
	ipamTypes "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/ipam/types"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s"
	v2 "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s/apis/cce.baidubce.com/v2"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/lock"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/logging"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/logging/logfields"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/math"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/trigger"
	v1 "k8s.io/api/core/v1"
)

const (
	// warningInterval is the interval for warnings which should be done
	// once and then repeated if the warning persists.
	warningInterval = time.Hour
)

// NetResource represents a Kubernetes node running CCE with an associated
// NetResourceSet custom resource
type NetResource struct {
	// mutex protects all members of this structure
	mutex lock.RWMutex

	// name is the name of the node
	name string

	// resource is the link to the NetResourceSet custom resource
	resource *v2.NetResourceSet

	// stats provides accounting for various per node statistics
	stats Statistics

	// lastMaxAdapterWarning is the timestamp when the last warning was
	// printed that this node is out of adapters
	lastMaxAdapterWarning time.Time

	// instanceRunning is true when the EC2 instance backing the node is
	// not running. This state is detected based on error messages returned
	// when modifying instance state
	instanceRunning bool

	// instanceStoppedRunning records when an instance was most recently set to not running
	instanceStoppedRunning time.Time

	// waitingForPoolMaintenance is true when the node is subject to an
	// IP allocation or release which must be performed before another
	// allocation or release can be attempted
	waitingForPoolMaintenance bool

	// resyncNeeded is set to the current time when a resync with the EC2
	// API is required. The timestamp is required to ensure that this is
	// only reset if the resync started after the time stored in
	// resyncNeeded. This is needed because resyncs and allocations happen
	// in parallel.
	resyncNeeded time.Time

	// available is the map of IPs available to this node
	available ipamTypes.AllocationMap

	// manager is the NodeManager responsible for this node
	manager *NetResourceSetManager

	// poolMaintainer is the trigger used to assign/unassign
	// private IP addresses of this node.
	// It ensures that multiple requests to operate private IPs are
	// batched together if pool maintenance is still ongoing.
	poolMaintainer *trigger.Trigger

	// k8sSync is the trigger used to synchronize node information with the
	// K8s apiserver. The trigger is used to batch multiple updates
	// together if the apiserver is slow to respond or subject to rate
	// limiting.
	k8sSync *trigger.Trigger

	// ops is the IPAM implementation to used for this node
	ops NetResourceOperations

	// retry is the trigger used to retry pool maintenance while the
	// instances API is unstable
	retry *trigger.Trigger

	// retry is the trigger used to retry syncToAPIServer while the
	// apiserver is unstable
	retrySyncToAPIServer *trigger.Trigger

	// Excess IPs from a cce node would be marked for release only after a delay configured by excess-ip-release-delay
	// flag. ipsMarkedForRelease tracks the IP and the timestamp at which it was marked for release.
	ipsMarkedForRelease map[string]time.Time

	// ipReleaseStatus tracks the state for every IP considered for release.
	// IPAMMarkForRelease  : Marked for Release
	// IPAMReadyForRelease : Acknowledged as safe to release by agent
	// IPAMDoNotRelease    : Release request denied by agent
	// IPAMReleased        : IP released by the operator
	ipReleaseStatus map[string]string

	// logLimiter rate limits potentially repeating warning logs
	logLimiter logging.Limiter
}

// Statistics represent the IP allocation statistics of a node
type Statistics struct {
	// UsedIPs is the number of IPs currently in use
	UsedIPs int

	// AvailableIPs is the number of IPs currently available for allocation
	// by the node
	AvailableIPs int

	// NeededIPs is the number of IPs needed to reach the PreAllocate
	// watermwark
	NeededIPs int

	// ExcessIPs is the number of free IPs exceeding MaxAboveWatermark
	ExcessIPs int

	// RemainingInterfaces is the number of interfaces that can either be
	// allocated or have not yet exhausted the instance specific quota of
	// addresses
	RemainingInterfaces int
}

// IsRunning returns true if the node is considered to be running
func (n *NetResource) IsRunning() bool {
	n.mutex.RLock()
	defer n.mutex.RUnlock()
	return n.instanceRunning && n.instanceStoppedRunning.Add(time.Minute).Before(time.Now())
}

func (n *NetResource) SetRunning() {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	if n.resource == nil {
		return
	}

	oldRunning := n.instanceRunning
	obj, err := k8s.CCEClient().Informers.Cce().V2().NetResourceSets().Lister().Get(n.resource.Name)
	if err != nil || obj == nil {
		n.instanceRunning = false
	} else if obj != nil {
		n.instanceRunning = true
	}

	n.loggerLocked().Debugf("Set running %t", n.instanceRunning)

	if oldRunning && !n.instanceRunning {
		n.instanceStoppedRunning = time.Now()
	}
}

// Stats returns a copy of the node statistics
func (n *NetResource) Stats() Statistics {
	n.mutex.RLock()
	c := n.stats
	n.mutex.RUnlock()
	return c
}

// Ops returns the IPAM implementation operations for the node
func (n *NetResource) Ops() NetResourceOperations {
	return n.ops
}

func (n *NetResource) IsPrefixDelegationEnabled() bool {
	if n == nil || n.manager == nil {
		return false
	}
	return n.manager.prefixDelegation
}

func (n *NetResource) logger() *logrus.Entry {
	if n == nil {
		return log
	}

	return n.loggerLocked()
}

func (n *NetResource) loggerLocked() (logger *logrus.Entry) {
	logger = log

	if n != nil {
		logger = logger.WithField(fieldName, n.name)
		if n.resource != nil {
			logger = logger.WithField("instanceID", n.resource.InstanceID())
		}
	}
	return
}

// getMaxAboveWatermark returns the max-above-watermark setting for an AWS node
//
// n.mutex must be held when calling this function
func (n *NetResource) getMaxAboveWatermark() int {
	return n.resource.Spec.IPAM.MaxAboveWatermark
}

// getPreAllocate returns the pre-allocation setting for an AWS node
//
// n.mutex must be held when calling this function
func (n *NetResource) getPreAllocate() int {
	if n.resource.Spec.IPAM.PreAllocate != 0 {
		return n.resource.Spec.IPAM.PreAllocate
	}
	return defaults.IPAMPreAllocation
}

// getMinAllocate returns the minimum-allocation setting of an AWS node
//
// n.mutex must be held when calling this function
func (n *NetResource) getMinAllocate() int {
	return n.resource.Spec.IPAM.MinAllocate
}

func (n *NetResource) getBurstableENIs() int {
	return n.resource.Spec.ENI.BurstableMehrfachENI
}

// getMaxAllocate returns the maximum-allocation setting of an AWS node
func (n *NetResource) getMaxAllocate() int {
	instanceMax := n.ops.GetMaximumAllocatableIPv4()
	if n.resource.Spec.IPAM.MaxAllocate > 0 {
		if n.resource.Spec.IPAM.MaxAllocate > instanceMax {
			n.loggerLocked().Debugf("max-allocate (%d) is higher than the instance type limits (%d)", n.resource.Spec.IPAM.MaxAllocate, instanceMax)
			return instanceMax
		}
		return n.resource.Spec.IPAM.MaxAllocate
	}

	return instanceMax
}

func (n *NetResource) getMaxIPBurstableIPCount() int {
	return n.ops.GetMaximumBurstableAllocatableIPv4()
}

// GetNeededAddresses returns the number of needed addresses that need to be
// allocated or released. A positive number is returned to indicate allocation.
// A negative number is returned to indicate release of addresses.
func (n *NetResource) GetNeededAddresses() int {
	stats := n.Stats()
	if stats.NeededIPs > 0 {
		return stats.NeededIPs
	}
	if n.manager.releaseExcessIPs && stats.ExcessIPs > 0 {
		// Nodes are sorted by needed addresses, return negative values of excessIPs
		// so that nodes with IP deficit are resolved first
		return stats.ExcessIPs * -1
	}
	return 0
}

// getPendingPodCount computes the number of pods in pending state on a given node. watchers.PodStore is assumed to be
// initialized before this function is called.
func getPendingPodCount(nodeName string) (int, error) {
	pendingPods := 0
	if watchers.PodStore == nil {
		return pendingPods, fmt.Errorf("pod store uninitialized")
	}
	values, err := watchers.PodStore.(cache.Indexer).ByIndex(watchers.PodNodeNameIndex, nodeName)
	if err != nil {
		return pendingPods, fmt.Errorf("unable to access pod to node name index: %w", err)
	}
	for _, pod := range values {
		p := pod.(*v1.Pod)
		if p.Status.Phase == v1.PodPending {
			pendingPods++
		}
	}
	return pendingPods, nil
}

func calculateNeededIPs(availableIPs, usedIPs, preAllocate, minAllocate, maxAllocate, burstableENIIPs int) (neededIPs int) {
	neededIPs = preAllocate - (availableIPs - usedIPs)

	if minAllocate > 0 {
		neededIPs = math.IntMax(neededIPs, minAllocate-availableIPs)
	}

	// Ensure that the total number of IP addresses applied for each time is not less than the IP
	// capacity of ENI if use burstableMehrfachENI
	if neededIPs > 0 && burstableENIIPs > 0 {
		neededIPs = burstableENIIPs
	}
	// If maxAllocate is set (> 0) and neededIPs is higher than the
	// maxAllocate value, we only return the amount of IPs that can
	// still be allocated
	if maxAllocate > 0 && (availableIPs+neededIPs) > maxAllocate {
		neededIPs = maxAllocate - availableIPs
	}

	if neededIPs < 0 {
		neededIPs = 0
	}
	return
}

func calculateExcessIPs(availableIPs, usedIPs, preAllocate, minAllocate, maxAboveWatermark, burstableMehrfachENI int) (excessIPs int) {
	// If burstableMehrfachENI is set, we do not need to calculate excessIPs
	if burstableMehrfachENI > 0 {
		return 0
	}
	// keep availableIPs above minAllocate + maxAboveWatermark as long as
	// the initial socket of min-allocate + max-above-watermark has not
	// been used up yet. This is the maximum potential allocation that will
	// happen on initial bootstrap.  Depending on interface restrictions,
	// the actual allocation may be below this but we always want to avoid
	// releasing IPs that have just been allocated.
	if usedIPs <= (minAllocate + maxAboveWatermark) {
		if availableIPs <= (minAllocate + maxAboveWatermark) {
			return 0
		}
	}

	// Once above the minimum allocation level, calculate based on
	// pre-allocation limit with the max-above-watermark limit calculated
	// in. This is again a best-effort calculation, depending on the
	// interface restrictions, less than max-above-watermark may have been
	// allocated but we never want to release IPs that have been allocated
	// because of max-above-watermark.
	excessIPs = availableIPs - usedIPs - preAllocate - maxAboveWatermark
	if excessIPs < 0 {
		excessIPs = 0
	}

	return
}

func (n *NetResource) requirePoolMaintenance() {
	n.mutex.Lock()
	n.waitingForPoolMaintenance = true
	n.mutex.Unlock()
}

func (n *NetResource) poolMaintenanceComplete() {
	n.mutex.Lock()
	n.waitingForPoolMaintenance = false
	n.mutex.Unlock()
}

// InstanceID returns the instance ID of the node
func (n *NetResource) InstanceID() (id string) {
	n.mutex.RLock()
	if n.resource != nil {
		id = n.resource.InstanceID()
	}
	n.mutex.RUnlock()
	return
}

// UpdatedResource is called when an update to the NetResourceSet has been
// received. The IPAM layer will attempt to immediately resolve any IP deficits
// and also trigger the background sync to continue working in the background
// to resolve any deficits or excess.
func (n *NetResource) UpdatedResource(resource *v2.NetResourceSet) bool {
	// Deep copy the resource before storing it. This way we are not
	// dependent on caller not using the resource after this call.
	if resource == nil {
		return false
	}
	resource = resource.DeepCopy()

	n.ops.UpdatedNode(resource)

	n.mutex.Lock()
	// Any modification to the custom resource is seen as a sign that the
	// instance is alive
	n.resource = resource
	n.mutex.Unlock()
	n.SetRunning()

	n.recalculate()
	allocationNeeded := n.allocationNeeded()
	if allocationNeeded {
		n.poolMaintainer.Trigger()
	}

	return allocationNeeded
}

func (n *NetResource) resourceAttached() (attached bool) {
	n.mutex.RLock()
	attached = n.resource != nil
	n.mutex.RUnlock()
	return
}

func (n *NetResource) recalculate() {
	// Skip any recalculation if the NetResourceSet resource does not exist yet
	if !n.resourceAttached() {
		return
	}
	scopedLog := n.logger()

	a, err := n.ops.ResyncInterfacesAndIPs(context.TODO(), scopedLog)

	n.mutex.Lock()
	defer n.mutex.Unlock()

	if err != nil {
		scopedLog.Warning("Instance not found! Please delete corresponding netresourceset if instance has already been deleted.")
		// Avoid any further action
		n.stats.NeededIPs = 0
		n.stats.ExcessIPs = 0
		return
	}

	n.available = a
	n.stats.UsedIPs = 0
	for ipstr := range n.resource.Status.IPAM.Used {
		ip := net.ParseIP(ipstr)
		if ip == nil || ip.To4() != nil {
			n.stats.UsedIPs++
		}
	}

	// Get used IP count with prefixes included
	usedIPForExcessCalc := n.stats.UsedIPs
	if n.ops.IsPrefixDelegated() {
		usedIPForExcessCalc = n.ops.GetUsedIPWithPrefixes()
	}

	availableIPv4Count := 0

	for ipstr := range n.available {
		ip := net.ParseIP(ipstr)
		if ip == nil || ip.To4() != nil {
			availableIPv4Count++
		}
	}
	n.stats.AvailableIPs = availableIPv4Count

	n.stats.NeededIPs = calculateNeededIPs(n.stats.AvailableIPs, n.stats.UsedIPs, n.getPreAllocate(), n.getMinAllocate(), n.getMaxAllocate(), n.getMaxIPBurstableIPCount())
	n.stats.ExcessIPs = calculateExcessIPs(n.stats.AvailableIPs, usedIPForExcessCalc, n.getPreAllocate(), n.getMinAllocate(), n.getMaxAboveWatermark(), n.getMaxIPBurstableIPCount())

	scopedLog.WithFields(logrus.Fields{
		"available":                 n.stats.AvailableIPs,
		"used":                      n.stats.UsedIPs,
		"toAlloc":                   n.stats.NeededIPs,
		"toRelease":                 n.stats.ExcessIPs,
		"waitingForPoolMaintenance": n.waitingForPoolMaintenance,
		"resyncNeeded":              n.resyncNeeded,
		"maxBurstableIPs":           n.getMaxIPBurstableIPCount(),
	}).Debug("Recalculated needed addresses")
}

// allocationNeeded returns true if this node requires IPs to be allocated
func (n *NetResource) allocationNeeded() (needed bool) {
	n.mutex.RLock()
	needed = !n.waitingForPoolMaintenance && n.resyncNeeded.IsZero() && n.stats.NeededIPs > 0
	n.mutex.RUnlock()
	return
}

// releaseNeeded returns true if this node requires IPs to be released
func (n *NetResource) releaseNeeded() (needed bool) {
	n.mutex.RLock()
	needed = n.manager.releaseExcessIPs && !n.waitingForPoolMaintenance && n.resyncNeeded.IsZero() && n.stats.ExcessIPs > 0
	if n.resource != nil {
		releaseInProgress := len(n.resource.Status.IPAM.ReleaseIPs) > 0
		needed = needed || releaseInProgress
	}
	n.mutex.RUnlock()
	return
}

// Pool returns the IP allocation pool available to the node
func (n *NetResource) Pool() (pool ipamTypes.AllocationMap) {
	pool = ipamTypes.AllocationMap{}
	n.mutex.RLock()
	for k, allocationIP := range n.available {
		pool[k] = allocationIP
	}
	n.mutex.RUnlock()
	return
}

// ResourceCopy returns a deep copy of the NetResourceSet custom resource
// associated with the node
func (n *NetResource) ResourceCopy() *v2.NetResourceSet {
	n.mutex.RLock()
	defer n.mutex.RUnlock()
	return n.resource.DeepCopy()
}

// createInterface creates an additional interface with the instance and
// attaches it to the instance as specified by the NetResourceSet. neededAddresses
// of secondary IPs are assigned to the interface up to the maximum number of
// addresses as allowed by the instance.
func (n *NetResource) createInterface(ctx context.Context, a *AllocationAction) (created bool, err error) {
	if a.AvailableInterfaces == 0 {
		// This is not a failure scenario, warn once per hour but do
		// not track as interface allocation failure. There is a
		// separate metric to track nodes running at capacity.
		n.mutex.Lock()
		if !n.lastMaxAdapterWarning.IsZero() && time.Since(n.lastMaxAdapterWarning) > warningInterval {
			n.loggerLocked().Warning("Instance is out of interfaces")
			n.lastMaxAdapterWarning = time.Now()
		}
		n.mutex.Unlock()
		return false, nil
	}

	scopedLog := n.logger()
	toAllocate, errCondition, err := n.ops.CreateInterface(ctx, a, scopedLog)
	if err != nil {
		scopedLog.Warningf("Unable to create interface on instance: %s", err)
		n.manager.metricsAPI.IncAllocationAttempt(errCondition, string(a.PoolID))
		return false, err
	}

	n.manager.metricsAPI.IncAllocationAttempt("success", string(a.PoolID))
	n.manager.metricsAPI.AddIPAllocation(string(a.PoolID), int64(toAllocate))

	return true, nil
}

// AllocationAction is the action to be taken to resolve allocation deficits
// for a particular node. It is returned by
// NodeOperations.PrepareIPAllocation() and passed into
// NodeOperations.AllocateIPs().
type AllocationAction struct {
	// InterfaceID is set to the identifier describing the interface on
	// which the IPs must be allocated. This is optional, an IPAM
	// implementation can leave this empty to indicate that no interface
	// context is needed or a new interface must be created.
	InterfaceID string

	// Interface is the interface to allocate IPs on
	Interface ipamTypes.InterfaceRevision

	// PoolID is the IPAM pool identifier to allocate the IPs from. This
	// can correspond to a subnet ID or it can also left blank or set to a
	// value such as "global" to indicate a single address pool.
	PoolID ipamTypes.PoolID

	// AvailableForAllocation is the number IPs available for allocation.
	// If InterfaeID is set, then this number corresponds to the number of
	// IPs available for allocation on that interface. This number may be
	// lower than the number of IPs required to resolve the deficit.
	AvailableForAllocationIPv4 int
	AvailableForAllocationIPv6 int

	// MaxIPsToAllocate is set by the core IPAM layer before
	// NodeOperations.AllocateIPs() is called and defines the maximum
	// number of IPs to allocate in order to stay within the boundaries as
	// defined by NodeOperations.{ MinAllocate() | PreAllocate() |
	// getMaxAboveWatermark() }.
	MaxIPsToAllocate int

	// AvailableInterfaces is the number of interfaces available to be created
	AvailableInterfaces int
}

// ReleaseAction is the action to be taken to resolve allocation excess for a
// particular node. It is returned by NodeOperations.PrepareIPRelease() and
// passed into NodeOperations.ReleaseIPs().
type ReleaseAction struct {
	// InterfaceID is set to the identifier describing the interface on
	// which the IPs must be released. This is optional, an IPAM
	// implementation can leave this empty to indicate that no interface
	// context is needed.
	InterfaceID string

	// PoolID is the IPAM pool identifier to release the IPs from. This can
	// correspond to a subnet ID or it can also left blank or set to a
	// value such as "global" to indicate a single address pool.
	PoolID ipamTypes.PoolID

	// IPsToRelease is the list of IPs to release
	IPsToRelease []string
}

// maintenanceAction represents the resources available for allocation for a
// particular netResourceSet. If an existing interface has IP allocation capacity
// left, that capacity is used up first. If not, an available index is found to
// create a new interface.
type maintenanceAction struct {
	allocation *AllocationAction
	release    *ReleaseAction
}

func (n *NetResource) determineMaintenanceAction() (*maintenanceAction, error) {
	var err error

	a := &maintenanceAction{}

	scopedLog := n.logger()
	stats := n.Stats()

	// Validate that the node still requires addresses to be released, the
	// request may have been resolved in the meantime.
	// we will disable the release of excess IPs for burstable ENI mode.
	// getMaxIPBurstableIPCount() == 0 meanes that we are not in burstable ENI mode.
	if n.manager.releaseExcessIPs && stats.ExcessIPs > 0 && n.getMaxIPBurstableIPCount() == 0 {
		a.release = n.ops.PrepareIPRelease(stats.ExcessIPs, scopedLog)
		return a, nil
	}

	// Validate that the node still requires addresses to be allocated, the
	// request may have been resolved in the meantime.
	if stats.NeededIPs == 0 {
		return nil, nil
	}

	a.allocation, err = n.ops.PrepareIPAllocation(scopedLog)
	if err != nil {
		return nil, err
	}

	surgeAllocate := 0
	numPendingPods, err := getPendingPodCount(n.name)
	if err != nil {
		if n.logLimiter.Allow() {
			scopedLog.WithError(err).Warningf("Unable to compute pending pods, will not surge-allocate")
		}
	} else if numPendingPods > stats.NeededIPs {
		surgeAllocate = numPendingPods - stats.NeededIPs
	}

	// handleIPAllocation() takes a min of MaxIPsToAllocate and IPs available for allocation on the interface.
	// This makes sure we don't try to allocate more than what's available.
	a.allocation.MaxIPsToAllocate = stats.NeededIPs + surgeAllocate

	if a.allocation != nil {
		n.mutex.Lock()
		n.stats.RemainingInterfaces = a.allocation.AvailableInterfaces
		stats = n.stats
		n.mutex.Unlock()
		scopedLog = scopedLog.WithFields(logrus.Fields{
			"selectedInterface":          a.allocation.InterfaceID,
			"selectedPoolID":             a.allocation.PoolID,
			"maxIPsToAllocate":           a.allocation.MaxIPsToAllocate,
			"availableForAllocationIPv4": a.allocation.AvailableForAllocationIPv4,
			"availableForAllocationIPv6": a.allocation.AvailableForAllocationIPv6,
			"availableInterfaces":        a.allocation.AvailableInterfaces,
		})
	}

	scopedLog.WithFields(logrus.Fields{
		"available":           stats.AvailableIPs,
		"used":                stats.UsedIPs,
		"neededIPs":           stats.NeededIPs,
		"remainingInterfaces": stats.RemainingInterfaces,
	}).Info("Resolving IP deficit of node")

	return a, nil
}

// removeStaleReleaseIPs Removes stale entries in local n.ipReleaseStatus. Once the handshake is complete agent would
// remove entries from IP release status map in netresourceset CRD's status. These IPs need to be purged from
// n.ipReleaseStatus
func (n *NetResource) removeStaleReleaseIPs() {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	for ip, status := range n.ipReleaseStatus {
		if status != ipamOption.IPAMReleased {
			continue
		}
		if _, ok := n.resource.Status.IPAM.ReleaseIPs[ip]; !ok {
			delete(n.ipReleaseStatus, ip)
		}
	}
}

// abortNoLongerExcessIPs allows for aborting release of IP if new allocations on the node result in a change of excess
// count or the interface selected for release.
func (n *NetResource) abortNoLongerExcessIPs(excessMap map[string]bool) {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	if len(n.resource.Status.IPAM.ReleaseIPs) == 0 {
		return
	}
	for ip, status := range n.resource.Status.IPAM.ReleaseIPs {
		if excessMap[ip] {
			continue
		}
		// Handshake can be aborted from every state except 'released'
		// 'released' state is removed by the agent once the IP has been removed from netresourceset's IPAM pool as well.
		if status == ipamOption.IPAMReleased {
			continue
		}
		if status, ok := n.ipReleaseStatus[ip]; ok && status != ipamOption.IPAMReleased {
			delete(n.ipsMarkedForRelease, ip)
			delete(n.ipReleaseStatus, ip)
		}
	}
}

// handleIPReleaseResponse handles IPs agent has already responded to
func (n *NetResource) handleIPReleaseResponse(markedIP string, ipsToRelease *[]string) bool {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	if n.resource.Status.IPAM.ReleaseIPs != nil {
		if status, ok := n.resource.Status.IPAM.ReleaseIPs[markedIP]; ok {
			switch status {
			case ipamOption.IPAMReadyForRelease:
				*ipsToRelease = append(*ipsToRelease, markedIP)
			case ipamOption.IPAMDoNotRelease:
				delete(n.ipsMarkedForRelease, markedIP)
				delete(n.ipReleaseStatus, markedIP)
			}
			// 'released' state is already handled in removeStaleReleaseIPs()
			// Other states don't need additional handling.
			return true
		}
	}
	return false
}

func (n *NetResource) deleteLocalReleaseStatus(ip string) {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	delete(n.ipReleaseStatus, ip)
}

// handleIPRelease implements IP release handshake needed for releasing excess IPs on the node.
// Operator initiates the handshake after an IP remains unused and excess for more than the number of seconds configured
// by excess-ip-release-delay flag. Operator uses a map in netresourceset's IPAM status field to exchange handshake
// information with the agent. Once the operator marks an IP for release, agent can either acknowledge or NACK IPs.
// If agent acknowledges, operator will release the IP and update the state to released. After the IP is removed from
// spec.ipam.pool and status is set to released, agent will remove the entry from map completing the handshake.
// Handshake is implemented with 4 states :
// * marked-for-release : Set by operator as possible candidate for IP
// * ready-for-release  : Acknowledged as safe to release by agent
// * do-not-release     : IP already in use / not owned by the node. Set by agent
// * released           : IP successfully released. Set by operator
//
// Handshake would be aborted if there are new allocations and the node doesn't have IPs in excess anymore.
func (n *NetResource) handleIPRelease(ctx context.Context, a *maintenanceAction) (instanceMutated bool, err error) {
	scopedLog := n.logger()
	var ipsToMark []string
	var ipsToRelease []string

	// Update timestamps for IPs from this iteration
	releaseTS := time.Now()
	if a.release != nil && a.release.IPsToRelease != nil {
		for _, ip := range a.release.IPsToRelease {
			if _, ok := n.ipsMarkedForRelease[ip]; !ok {
				n.ipsMarkedForRelease[ip] = releaseTS
			}
		}
	}

	if n.ipsMarkedForRelease == nil || a.release == nil || len(a.release.IPsToRelease) == 0 {
		// Resetting ipsMarkedForRelease if there are no IPs to release in this iteration
		n.ipsMarkedForRelease = make(map[string]time.Time)
	}

	for markedIP, ts := range n.ipsMarkedForRelease {
		// Determine which IPs are still marked for release.
		stillMarkedForRelease := false
		for _, ip := range a.release.IPsToRelease {
			if markedIP == ip {
				stillMarkedForRelease = true
				break
			}
		}
		if !stillMarkedForRelease {
			// n.determineMaintenanceAction() only returns the IPs on the interface with maximum number of IPs that
			// can be freed up. If the selected interface changes or if this IP is not excess anymore, remove entry
			// from local maps.
			delete(n.ipsMarkedForRelease, markedIP)
			n.deleteLocalReleaseStatus(markedIP)
			continue
		}
		// Check if the IP release waiting period elapsed
		if ts.Add(time.Duration(operatorOption.Config.ExcessIPReleaseDelay) * time.Second).After(time.Now()) {
			continue
		}
		// Handling for IPs we've already heard back from agent.
		if n.handleIPReleaseResponse(markedIP, &ipsToRelease) {
			continue
		}
		// markedIP can now be considered excess and is not currently in an active handshake
		ipsToMark = append(ipsToMark, markedIP)
	}

	n.mutex.Lock()
	for _, ip := range ipsToMark {
		scopedLog.WithFields(logrus.Fields{logfields.IPAddr: ip}).Debug("Marking IP for release")
		n.ipReleaseStatus[ip] = ipamOption.IPAMMarkForRelease
	}
	n.mutex.Unlock()

	// Abort handshake for IPs that are in the middle of handshake, but are no longer considered excess
	var excessMap map[string]bool
	if a.release != nil && len(a.release.IPsToRelease) > 0 {
		excessMap = make(map[string]bool, len(a.release.IPsToRelease))
		for _, ip := range a.release.IPsToRelease {
			excessMap[ip] = true
		}
	}
	n.abortNoLongerExcessIPs(excessMap)

	if len(ipsToRelease) > 0 {
		a.release.IPsToRelease = ipsToRelease
		scopedLog = scopedLog.WithFields(logrus.Fields{
			"available":         n.stats.AvailableIPs,
			"used":              n.stats.UsedIPs,
			"excess":            n.stats.ExcessIPs,
			"excessIps":         a.release.IPsToRelease,
			"releasing":         ipsToRelease,
			"selectedInterface": a.release.InterfaceID,
			"selectedPoolID":    a.release.PoolID,
		})
		scopedLog.Info("Releasing excess IPs from node")
		err := n.ops.ReleaseIPs(ctx, a.release)
		if err == nil {
			n.manager.metricsAPI.AddIPRelease(string(a.release.PoolID), int64(len(a.release.IPsToRelease)))

			// Remove the IPs from ipsMarkedForRelease
			n.mutex.Lock()
			for _, ip := range ipsToRelease {
				delete(n.ipsMarkedForRelease, ip)
				n.ipReleaseStatus[ip] = ipamOption.IPAMReleased
			}
			n.mutex.Unlock()
			return true, nil
		}
		n.manager.metricsAPI.IncAllocationAttempt("ip unassignment failed", string(a.release.PoolID))
		scopedLog.WithFields(logrus.Fields{
			"selectedInterface":  a.release.InterfaceID,
			"releasingAddresses": len(a.release.IPsToRelease),
		}).WithError(err).Warning("Unable to unassign IPs from interface")
		return false, err
	}
	return false, nil
}

// handleIPAllocation allocates the necessary IPs needed to resolve deficit on the node.
// If existing interfaces don't have enough capacity, new interface would be created.
func (n *NetResource) handleIPAllocation(ctx context.Context, a *maintenanceAction) (instanceMutated bool, err error) {
	scopedLog := n.logger()
	if a.allocation == nil {
		scopedLog.Debug("No allocation action required")
		return false, nil
	}

	// Assign needed addresses when use ippool
	if a.allocation.AvailableForAllocationIPv4 > 0 || a.allocation.AvailableForAllocationIPv6 > 0 {
		maxIPNum := a.allocation.MaxIPsToAllocate
		a.allocation.AvailableForAllocationIPv4 = math.IntMin(a.allocation.AvailableForAllocationIPv4, maxIPNum)
		a.allocation.AvailableForAllocationIPv6 = math.IntMin(a.allocation.AvailableForAllocationIPv6, maxIPNum)

		err := n.ops.AllocateIPs(ctx, a.allocation)
		if err == nil {
			n.manager.metricsAPI.IncAllocationAttempt("success", string(a.allocation.PoolID))
			n.manager.metricsAPI.AddIPAllocation(string(a.allocation.PoolID), int64(a.allocation.AvailableForAllocationIPv6+a.allocation.AvailableForAllocationIPv4))
			return true, nil
		}

		n.manager.metricsAPI.IncAllocationAttempt("ip assignment failed", string(a.allocation.PoolID))
		scopedLog.WithFields(logrus.Fields{
			"selectedInterface": a.allocation.InterfaceID,
			"ipsToAllocateIPv4": a.allocation.AvailableForAllocationIPv4,
			"ipsToAllocateIPv6": a.allocation.AvailableForAllocationIPv4,
		}).WithError(err).Warning("Unable to assign additional IPs to interface, will create new interface")
	}

	// case 1 : create new interface when use ippool mode
	// case 2 : create new interface and assign IPs when use bursable ENI mode
	return n.createInterface(ctx, a.allocation)
}

// maintainIPPool attempts to allocate or release all required IPs to fulfill the needed gap.
// returns instanceMutated which tracks if state changed with the cloud provider and is used
// to determine if IPAM pool maintainer trigger func needs to be invoked.
func (n *NetResource) maintainIPPool(ctx context.Context) (instanceMutated bool, err error) {
	if n.manager.releaseExcessIPs {
		n.removeStaleReleaseIPs()
	}

	a, err := n.determineMaintenanceAction()
	if err != nil {
		n.abortNoLongerExcessIPs(nil)
		return false, err
	}

	// Maintenance request has already been fulfilled
	if a == nil {
		n.abortNoLongerExcessIPs(nil)
		return false, nil
	}

	if instanceMutated, err := n.handleIPRelease(ctx, a); instanceMutated || err != nil {
		return instanceMutated, err
	}

	return n.handleIPAllocation(ctx, a)
}

func (n *NetResource) requireResync() {
	n.mutex.Lock()
	n.resyncNeeded = time.Now()
	n.mutex.Unlock()
}

func (n *NetResource) updateLastResync(syncTime time.Time) {
	n.mutex.Lock()
	if syncTime.After(n.resyncNeeded) {
		n.loggerLocked().Debug("Resetting resyncNeeded")
		n.resyncNeeded = time.Time{}
	}
	n.mutex.Unlock()
}

// MaintainIPPool attempts to allocate or release all required IPs to fulfill
// the needed gap. If required, interfaces are created.
func (n *NetResource) MaintainIPPool(ctx context.Context) error {
	// it is neccessary to update pool maintenance time before the actual
	// this operation can effectively reduce duplicate synchronization
	n.requirePoolMaintenance()
	defer n.poolMaintenanceComplete()

	// As long as the instances API is unstable, don't perform any
	// operation that can mutate state.
	if !n.manager.InstancesAPIIsReady() {
		if n.retry != nil {
			n.retry.Trigger()
		}
		return fmt.Errorf("instances API is unstable. Blocking mutating operations. See logs for details")
	}

	// If the instance has stopped running for less than a minute, don't attempt any deficit
	// resolution and wait for the custom resource to be updated as a sign
	// of life.
	if !n.IsRunning() {
		return nil
	}

	instanceMutated, err := n.maintainIPPool(ctx)
	if err == nil {
		n.logger().Debug("Setting resync needed")
		n.requireResync()
	}
	n.recalculate()
	if instanceMutated || err != nil {
		n.logger().Debug("MaintainIPPool triggering resync")
		n.manager.resyncTrigger.Trigger()
	}
	return err
}

// PopulateIPReleaseStatus Updates cce node IPAM status with excess IP release data
func (n *NetResource) PopulateIPReleaseStatus(node *v2.NetResourceSet) {
	// maintainIPPool() might not have run yet since the last update from agent.
	// Attempt to remove any stale entries
	n.removeStaleReleaseIPs()
	n.mutex.Lock()
	defer n.mutex.Unlock()
	releaseStatus := make(map[string]ipamTypes.IPReleaseStatus)
	for ip, status := range n.ipReleaseStatus {
		if existingStatus, ok := node.Status.IPAM.ReleaseIPs[ip]; ok && status == ipamOption.IPAMMarkForRelease {
			// retain status if agent already responded to this IP
			if existingStatus == ipamOption.IPAMReadyForRelease || existingStatus == ipamOption.IPAMDoNotRelease {
				releaseStatus[ip] = existingStatus
				continue
			}
		}
		releaseStatus[ip] = ipamTypes.IPReleaseStatus(status)
	}
	node.Status.IPAM.ReleaseIPs = releaseStatus
}

// syncToAPIServer synchronizes the contents of the NetResourceSet resource
// [(*Node).resource)] with the K8s apiserver. This operation occurs on an
// interval to refresh the NetResourceSet resource.
//
// For Azure and ENI IPAM modes, this function serves two purposes: (1)
// finalizes the initialization of the NetResourceSet resource (setting
// PreAllocate) and (2) to keep the resource up-to-date with K8s.
//
// To initialize, or seed, the NetResourceSet resource, the PreAllocate field is
// populated with a default value and then is adjusted as necessary.
func (n *NetResource) syncToAPIServer() (err error) {
	scopedLog := n.logger()
	scopedLog.Debug("Refreshing node")

	defer func() {
		if err != nil {
			n.retrySyncToAPIServer.Trigger()
			scopedLog.WithError(err).Error("Error syncing NetworkResourceSet to apiserver, triggering retrySyncToAPIServer.Trigger()")
		}
	}()

	node := n.ResourceCopy()
	// n.resource may not have been assigned yet
	if node == nil {
		return
	}

	origNode := node.DeepCopy()

	// We create a snapshot of the IP pool before we update the status. This
	// ensures that the pool in the spec is always older than the IPAM
	// information in the status.
	// This ordering is important, because otherwise a new IP could be added to
	// the pool after we updated the status, thereby creating a situation where
	// the agent does not have the necessary IPAM information to use the newly
	// added IP.
	// When an IP is removed, this is also safe. IP release is done via
	// handshake, where the agent will never use any IP where it has
	// acknowledged the release handshake. Therefore, having an already
	// released IP in the pool is fine, as the agent will ignore it.
	pool := n.Pool()

	// Always update the status first to ensure that the IPAM information
	// is synced for all addresses that are marked as available.
	//
	// Two attempts are made in case the local resource is outdated. If the
	// second attempt fails as well we are likely under heavy contention,
	// fall back to the controller based background interval to retry.
	for retry := 0; retry < 2; retry++ {
		if node.Status.IPAM.Used == nil {
			node.Status.IPAM.Used = ipamTypes.AllocationMap{}
		}

		n.ops.PopulateStatusFields(node)
		n.PopulateIPReleaseStatus(node)

		err = n.update(origNode, node, retry, true)
		if err == nil {
			break
		}
		if kerrors.IsNotFound(err) {
			scopedLog.WithError(err).Warning("skipping to update NetResourceSet status")
			return nil
		}
	}

	if err != nil {
		scopedLog.WithError(err).Warning("Unable to update NetResourceSet status")
		return err
	}

	for retry := 0; retry < 2; retry++ {
		node.Spec.IPAM.Pool = pool
		scopedLog.WithField("poolSize", len(node.Spec.IPAM.Pool)).Debug("Updating node in apiserver")

		// The PreAllocate value is added here rather than where the NetResourceSet
		// resource is created ((*NodeDiscovery).mutateNodeResource() inside
		// pkg/nodediscovery), because mutateNodeResource() does not have
		// access to the ipam.Node object. Since we are in the NetResourceSet
		// update sync loop, we can compute the value.
		if node.Spec.IPAM.PreAllocate == 0 {
			node.Spec.IPAM.PreAllocate = n.ops.GetMinimumAllocatableIPv4()
		}

		err = n.update(origNode, node, retry, false)
		if err == nil {
			break
		}
	}

	if err != nil {
		scopedLog.WithError(err).Warning("Unable to update NetResourceSet spec")
	}

	return err
}

// update is a helper function for syncToAPIServer(). This function updates the
// NetResourceSet resource spec or status depending on `status`. The resource is
// updated from `origNode` to `node`.
//
// Note that the `origNode` and `node` pointers will have their underlying
// values modified in this function! The following is an outline of when
// `origNode` and `node` pointers are updated:
//   - `node` is updated when we succeed in updating to update the resource to
//     the apiserver.
//   - `origNode` and `node` are updated when we fail to update the resource,
//     but we succeed in retrieving the latest version of it from the
//     apiserver.
func (n *NetResource) update(origNode, node *v2.NetResourceSet, attempts int, status bool) error {
	scopedLog := n.logger()

	var (
		updatedNode    *v2.NetResourceSet
		updateErr, err error
	)

	if status {
		updatedNode, updateErr = n.manager.k8sAPI.UpdateStatus(origNode, node)
	} else {
		updatedNode, updateErr = n.manager.k8sAPI.Update(origNode, node)
	}

	if updatedNode != nil && updatedNode.Name != "" {
		*node = *updatedNode
		if updateErr == nil {
			return nil
		}
	} else if updateErr != nil {
		scopedLog.WithError(updateErr).WithFields(logrus.Fields{
			logfields.Attempt: attempts,
			"updateStatus":    status,
		}).Debug("Failed to update NetResourceSet")

		var newNode *v2.NetResourceSet
		newNode, err = n.manager.k8sAPI.Get(node.Name)
		if err != nil {
			return err
		}

		// Propagate the error in the case that we are on our last attempt and
		// we never succeeded in updating the resource.
		//
		// Also, propagate the reference to the nodes in the case we've
		// succeeded in updating the NetResourceSet status. The reason is because
		// the subsequent run will be to update the NetResourceSet spec and we need
		// to ensure we have the most up-to-date NetResourceSet references before
		// doing that operation, hence the deep copies.
		err = updateErr
		*node = *newNode
		*origNode = *node
	} else /* updateErr == nil */ {
		err = updateErr
	}

	return err
}
