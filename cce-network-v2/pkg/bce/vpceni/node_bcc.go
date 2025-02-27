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
package vpceni

import (
	"context"
	"fmt"
	"sync"

	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/api/v1/models"
	operatorOption "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/operator/option"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/operator/watchers"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/bce/api/metadata"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/bce/bcesync"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/ipam"
	ipamTypes "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/ipam/types"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s"
	ccev2 "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s/apis/cce.baidubce.com/v2"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/logging/logfields"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/math"
	bccapi "github.com/baidubce/bce-sdk-go/services/bcc/api"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/cache"
)

// bccNetworkResourceSet is a wrapper of bceNetworkResourceSet, which is used to distinguish bcc node
type bccNetworkResourceSet struct {
	*bceNetworkResourceSet

	// bcc instance info
	bccInfo *bccapi.InstanceModel
	bccLock sync.RWMutex

	// usePrimaryENIWithSecondaryMode primary eni with secondary IP mode only use by ebc instance
	usePrimaryENIWithSecondaryMode bool
}

func newBCCNetworkResourceSet(super *bceNetworkResourceSet) *bccNetworkResourceSet {
	node := &bccNetworkResourceSet{
		bceNetworkResourceSet: super,
	}
	node.instanceType = string(metadata.InstanceTypeExBCC)
	return node
}

// refreshBCCInfo refresh bcc instance info
// caller should ensure be locked
func (n *bccNetworkResourceSet) refreshBCCInfo() (*bccapi.InstanceModel, error) {
	n.bccLock.RLock()
	if n.bccInfo != nil {
		n.bccLock.RUnlock()
		return n.bccInfo, nil
	}
	n.bccLock.RUnlock()

	bccInfo, err := n.manager.bceclient.GetBCCInstanceDetail(context.TODO(), n.instanceID)
	if err != nil {
		n.log.Errorf("faild to get bcc instance detail: %v", err)
		return n.bccInfo, err
	}
	n.log.WithField("bccInfo", logfields.Repr(bccInfo)).Infof("Get bcc instance detail")

	if bccInfo.EniQuota == 0 {
		n.usePrimaryENIWithSecondaryMode = true
	}

	n.bccLock.Lock()
	defer n.bccLock.Unlock()
	// add security group to node
	if operatorOption.Config.SecurityGroupSynerDuration > 0 {
		n.securityGroupIDs = bccInfo.NicInfo.SecurityGroups
		esgs, err := n.manager.bceclient.ListEsg(context.TODO(), n.instanceID)
		if err != nil {
			n.log.WithError(err).Error("list esg failed")
		} else {
			n.enterpriseSecurityGroupIDs = make([]string, 0)
			for _, esg := range esgs {
				n.enterpriseSecurityGroupIDs = append(n.securityGroupIDs, esg.Id)
			}
		}
	}

	n.bccInfo = bccInfo
	return n.bccInfo, nil
}

func (n *bccNetworkResourceSet) refreshENIQuota(scopeLog *logrus.Entry) (ENIQuotaManager, error) {
	scopeLog = scopeLog.WithField("nodeName", n.k8sObj.Name).WithField("method", "getENIQuota")
	client := k8s.WatcherClient()
	if client == nil {
		scopeLog.Fatal("K8s client is nil")
	}
	k8sNode, err := client.Informers.Core().V1().Nodes().Lister().Get(n.k8sObj.Name)
	if err != nil {
		scopeLog.Errorf("Get node failed: %v", err)
		return nil, err
	}

	bccInfo, err := n.refreshBCCInfo()
	if err != nil {
		return nil, err
	}

	eniQuota := newCustomerIPQuota(scopeLog, client, n.k8sObj.Name, n.instanceID, n.manager.bceclient)
	// default bbc ip quota
	defaltENINums, defaultIPs := getDefaultBCCEniQuota(k8sNode)

	// Expect all BCC models to support ENI
	// EBC models may not support eni, and there are also some non console created
	// BCCs that do not support this parameter
	if bccInfo.EniQuota != 0 || n.k8sObj.Spec.ENI.InstanceType == string(ccev2.ENIForBCC) {
		eniQuota.SetMaxENI(bccInfo.EniQuota)
		if bccInfo.EniQuota == 0 {
			eniQuota.SetMaxENI(defaltENINums)
		}
	}
	eniQuota.SetMaxIP(defaultIPs)

	// if node use primary ENI mode, there is no need to check IP resouce
	if n.k8sObj.Spec.ENI.UseMode == string(ccev2.ENIUseModePrimaryIP) {
		eniQuota.SetMaxIP(0)
	}

	return eniQuota, nil
}

func getDefaultBCCEniQuota(k8sNode *corev1.Node) (int, int) {
	var (
		cpuNum, memGB int
	)
	if cpu, ok := k8sNode.Status.Capacity[corev1.ResourceCPU]; ok {
		cpuNum = int(cpu.ScaledValue(resource.Milli)) / 1000
	}
	if mem, ok := k8sNode.Status.Capacity[corev1.ResourceMemory]; ok {
		memGB = int(mem.Value() / 1024 / 1024 / 1024)
	}
	return calculateMaxENIPerNode(cpuNum), calculateMaxIPPerENI(memGB)
}

// PrepareIPAllocation is called to calculate the number of IPs that
// can be allocated on the node and whether a new network interface
// must be attached to the node.
func (n *bccNetworkResourceSet) prepareIPAllocation(scopedLog *logrus.Entry) (a *ipam.AllocationAction, err error) {
	_, err = n.refreshBCCInfo()
	if err != nil {
		scopedLog.Errorf("failed to refresh ebc info: %v", err)
		return nil, fmt.Errorf("failed to refresh ebc info")
	}
	return n.__prepareIPAllocation(scopedLog, true)
}

func (n *bccNetworkResourceSet) __prepareIPAllocation(scopedLog *logrus.Entry, checkSubnet bool) (a *ipam.AllocationAction, err error) {
	a = &ipam.AllocationAction{}
	eniQuota := n.bceNetworkResourceSet.getENIQuota()
	if eniQuota == nil {
		return nil, fmt.Errorf("eniQuota is nil, please retry later")
	}
	eniCount := 0

	err = n.manager.ForeachInstance(n.instanceID, n.k8sObj.Name,
		func(instanceID, interfaceID string, iface ipamTypes.InterfaceRevision) error {
			// available eni have been found
			eniCount++

			if (a.AvailableForAllocationIPv4 > 0 || a.AvailableForAllocationIPv6 > 0) &&
				a.PoolID != "" &&
				a.InterfaceID != "" {
				return nil
			}

			e, ok := iface.Resource.(*eniResource)
			if !ok {
				return nil
			}

			scopedLog = scopedLog.WithFields(logrus.Fields{
				"eniID":        interfaceID,
				"index":        e.Status.InterfaceIndex,
				"numAddresses": len(e.Spec.ENI.PrivateIPSet) - 1,
			})
			if e.Spec.UseMode == ccev2.ENIUseModePrimaryIP {
				return nil
			}

			// Eni that is not in an in use state should not be ignored.
			// Otherwise, this will result in the creation of a new eni.
			if e.Status.VPCStatus != ccev2.VPCENIStatusInuse {
				return fmt.Errorf("can not prepare ip allocation. eni %s is not inuse on vpc, please try again later", interfaceID)
			}
			// The limits include the primary IP, so we need to take it into account
			// when computing the effective number of available addresses on the ENI.
			effectiveLimits := eniQuota.GetMaxIP()
			scopedLog.WithFields(logrus.Fields{
				"addressLimit": effectiveLimits,
			}).Debug("Considering ENI for allocation")

			amap := ipamTypes.AllocationMap{}
			addAllocationIP := func(ips []*models.PrivateIP) {
				for _, addr := range ips {
					// filter cross subnet private ip
					if addr.SubnetID != e.Spec.ENI.SubnetID {
						amap[addr.PrivateIPAddress] = ipamTypes.AllocationIP{Resource: e.Spec.ENI.ID}
					}
				}
			}
			addAllocationIP(e.Spec.ENI.PrivateIPSet)

			var (
				availableIPv4OnENI = 0
				availableIPv6OnENI = 0
			)
			if operatorOption.Config.EnableIPv4 && operatorOption.Config.EnableIPv6 {
				// Align the total number of IPv4 and IPv6
				ipv6Diff := len(e.Spec.ENI.IPV6PrivateIPSet) - len(e.Spec.ENI.PrivateIPSet)
				if ipv6Diff > 0 {
					availableIPv4OnENI = math.IntMin(effectiveLimits-len(e.Spec.ENI.PrivateIPSet), ipv6Diff)
				} else if ipv6Diff < 0 {
					availableIPv6OnENI = math.IntMin(effectiveLimits-len(e.Spec.ENI.IPV6PrivateIPSet), -ipv6Diff)
				} else {
					availableIPv4OnENI = math.IntMax(effectiveLimits-len(e.Spec.ENI.PrivateIPSet), 0)
					availableIPv6OnENI = math.IntMax(effectiveLimits-len(e.Spec.ENI.IPV6PrivateIPSet), 0)
				}
			} else if operatorOption.Config.EnableIPv4 {
				availableIPv4OnENI = math.IntMax(effectiveLimits-len(e.Spec.ENI.PrivateIPSet), 0)
			} else if operatorOption.Config.EnableIPv6 {
				availableIPv6OnENI = math.IntMax(effectiveLimits-len(e.Spec.ENI.IPV6PrivateIPSet), 0)
			}

			if availableIPv4OnENI == 0 && availableIPv6OnENI == 0 {
				return nil
			}

			scopedLog.WithFields(logrus.Fields{
				"eniID":              interfaceID,
				"availableIPv4OnENI": availableIPv4OnENI,
				"availableIPv6OnENI": availableIPv6OnENI,
			}).Debug("ENI has IPs available")

			if !checkSubnet {
				a.InterfaceID = interfaceID
				a.PoolID = ipamTypes.PoolID(e.Spec.ENI.SubnetID)
				a.AvailableForAllocationIPv4 = availableIPv4OnENI
				a.AvailableForAllocationIPv6 = availableIPv6OnENI
				return nil
			}

			if subnet, err := bcesync.GlobalBSM().GetSubnet(e.Spec.ENI.SubnetID); err == nil {
				if subnet.GetBorrowedIPNum(interfaceID) > 0 {
					a.AvailableForAllocationIPv4 = math.IntMin(subnet.GetBorrowedIPNum(interfaceID), availableIPv4OnENI)
				} else {
					a.AvailableForAllocationIPv4 = math.IntMin(subnet.BorrowedAvailableIPsCount, availableIPv4OnENI)
				}
				a.InterfaceID = interfaceID
				a.PoolID = ipamTypes.PoolID(subnet.Name)
				// does not need to be checked for subnet with ipv6
				a.AvailableForAllocationIPv6 = availableIPv6OnENI
			} else {
				return fmt.Errorf("get subnet by id %s failed: %v", e.Spec.ENI.SubnetID, err)
			}
			return nil
		})
	a.AvailableInterfaces = math.IntMax(eniQuota.GetMaxENI()-eniCount, 0)
	return
}

// AllocateIPs is called after invoking PrepareIPAllocation and needs
// to perform the actual allocation.
func (n *bccNetworkResourceSet) allocateIPs(ctx context.Context, scopedLog *logrus.Entry, allocation *ipam.AllocationAction, ipv4ToAllocate, ipv6ToAllocate int) (
	ipv4PrivateIPSet, ipv6PrivateIPSet []*models.PrivateIP, err error) {
	var ips []string
	if ipv4ToAllocate > 0 {
		// allocate ip
		ips, err = n.manager.bceclient.BatchAddPrivateIP(ctx, []string{}, ipv4ToAllocate, allocation.InterfaceID, false)
		err = n.manager.HandlerVPCError(scopedLog, err, string(allocation.PoolID))
		if err != nil {
			return nil, nil, fmt.Errorf("allocate ip to eni %s failed: %v", allocation.InterfaceID, err)
		}
		scopedLog.WithField("ips", ips).Debug("allocate ip to eni success")

		for _, ipstring := range ips {
			ipv4PrivateIPSet = append(ipv4PrivateIPSet, &models.PrivateIP{
				PrivateIPAddress: ipstring,
				SubnetID:         string(allocation.PoolID),
			})
		}
	}

	// allocate ipv6
	if ipv6ToAllocate > 0 {
		// allocate ipv6
		ips, err = n.manager.bceclient.BatchAddPrivateIP(ctx, []string{}, ipv6ToAllocate, allocation.InterfaceID, true)
		err = n.manager.HandlerVPCError(scopedLog, err, string(allocation.PoolID))
		if err != nil {
			err = fmt.Errorf("allocate ipv6 to eni %s failed: %v", allocation.InterfaceID, err)
			return
		}
		for _, ipstring := range ips {
			ipv6PrivateIPSet = append(ipv6PrivateIPSet, &models.PrivateIP{
				PrivateIPAddress: ipstring,
				SubnetID:         string(allocation.PoolID),
			})
		}
		scopedLog.WithField("ips", ips).Debug("allocate ipv6 to eni success")
	}
	return
}

// ReleaseIPs is called after invoking PrepareIPRelease and needs to
// perform the release of IPs.
func (n *bccNetworkResourceSet) releaseIPs(ctx context.Context, release *ipam.ReleaseAction, ipv4ToRelease, ipv6ToRelease []string) error {
	if len(ipv4ToRelease) > 0 {
		err := n.manager.bceclient.BatchDeletePrivateIP(ctx, ipv4ToRelease, release.InterfaceID, false)
		if err != nil {
			return fmt.Errorf("release ipv4 %v from eni %s failed: %v", ipv4ToRelease, release.InterfaceID, err)
		}
	}
	if len(ipv6ToRelease) > 0 {
		err := n.manager.bceclient.BatchDeletePrivateIP(ctx, ipv6ToRelease, release.InterfaceID, true)
		if err != nil {
			return fmt.Errorf("release ipv6 %v from eni %s failed: %v", ipv6ToRelease, release.InterfaceID, err)
		}
	}
	return nil
}

// AllocateIPCrossSubnet implements realNodeInf
func (n *bccNetworkResourceSet) allocateIPCrossSubnet(ctx context.Context, sbnID string) (result []*models.PrivateIP, eniID string, err error) {
	if n.k8sObj.Spec.ENI.UseMode == string(ccev2.ENIUseModePrimaryIP) {
		return nil, "", fmt.Errorf("allocate ip cross subnet not support primary ip mode")
	}
	scopedLog := n.log.WithField("subnet", sbnID).WithField("action", "allocateIPCrossSubnet")

	// get eni
	action, err := n.__prepareIPAllocation(scopedLog, false)
	if err != nil {
		return nil, "", err
	}
	if action.AvailableForAllocationIPv4 == 0 && action.AvailableForAllocationIPv6 == 0 {
		if action.AvailableInterfaces == 0 {
			return nil, "", fmt.Errorf("no available ip for allocation on node %s", n.k8sObj.Name)
		}
		_, eniID, err = n.CreateInterface(ctx, action, scopedLog)
		if err != nil {
			return nil, "", fmt.Errorf("create interface failed: %v", err)
		}
		return nil, "", fmt.Errorf("no available eni for allocation on node %s, try to create new eni %s", n.k8sObj.Name, eniID)
	}
	eniID = action.InterfaceID
	scopedLog = scopedLog.WithField("eni", eniID)
	scopedLog.Debug("prepare allocate ip cross subnet for eni")

	sbn, err := n.manager.sbnlister.Get(sbnID)
	if err != nil {
		return nil, eniID, fmt.Errorf("get subnet %s failed: %v", sbnID, err)
	}

	var (
		ipstr      []string
		ipv4Result *models.PrivateIP
		ipv6Result *models.PrivateIP
	)

	defer func() {
		// to roll back ip
		if err != nil {
			var ipsToRelease []string
			if ipv4Result != nil {
				ipsToRelease = append(ipsToRelease, ipv4Result.PrivateIPAddress)
			}
			if ipv6Result != nil {
				ipsToRelease = append(ipsToRelease, ipv6Result.PrivateIPAddress)
			}
			if len(ipsToRelease) > 0 {
				err2 := n.manager.bceclient.BatchDeletePrivateIP(ctx, ipsToRelease, eniID, true)
				if err2 != nil {
					scopedLog.WithError(err2).Error("release ip failed")
				}
			}
		}
	}()

	// allocate ipv4
	ipstr, err = n.manager.bceclient.BatchAddPrivateIpCrossSubnet(ctx, eniID, sbnID, []string{}, 1, false)
	if n.manager.HandlerVPCError(scopedLog, err, sbnID) != nil {
		scopedLog.WithError(err).Error("allocate ip cross subnet failed")
		return
	}
	if len(ipstr) != 1 {
		err = fmt.Errorf("allocate ip cross subnet failed, ipstr len %d", len(ipstr))
		scopedLog.WithError(err).Error("allocate ip cross subnet failed")
		return
	}
	ipv4Result = &models.PrivateIP{
		PrivateIPAddress: ipstr[0],
		SubnetID:         sbnID,
		Primary:          false,
	}
	result = append(result, ipv4Result)
	scopedLog.WithField("ipv4", ipstr).Debug("allocate ipv4 cross subnet success")

	if operatorOption.Config.EnableIPv6 && sbn.Spec.IPv6CIDR != "" {
		ipstr, err = n.manager.bceclient.BatchAddPrivateIpCrossSubnet(ctx, eniID, sbnID, []string{}, 1, true)
		if n.manager.HandlerVPCError(scopedLog, err, sbnID) != nil {
			scopedLog.WithError(err).Error("allocate ipv6 cross subnet failed")
			return
		}
		if len(ipstr) != 1 {
			err = fmt.Errorf("allocate ipv6 cross subnet failed, ipstr len %d", len(ipstr))
			scopedLog.WithError(err).Error("allocate ipv6 cross subnet failed")
			return
		}
		ipv6Result = &models.PrivateIP{
			PrivateIPAddress: ipstr[0],
			SubnetID:         sbnID,
			Primary:          false,
		}
		result = append(result, ipv6Result)
		scopedLog.WithField("ipv6", ipstr).Debug("allocate ipv6 cross subnet success")
	}
	return
}

// ReuseIPs implements realNodeInf
func (n *bccNetworkResourceSet) reuseIPs(ctx context.Context, ips []*models.PrivateIP, owner string) (eniID string, err error) {
	if n.k8sObj.Spec.ENI.UseMode == string(ccev2.ENIUseModePrimaryIP) {
		return "", fmt.Errorf("allocate ip cross subnet not support primary ip mode")
	}
	scopedLog := n.log.WithField("action", "reuseIPs")
	if len(ips) == 0 {
		return "", fmt.Errorf("no ip to reuse")
	}

	namespace, name, err := cache.SplitMetaNamespaceKey(owner)
	if err != nil {
		err = fmt.Errorf("invalid owner %s: %v", owner, err)
		return
	}
	scopedLog = scopedLog.WithField("owner", owner)
	var (
		ipstr      []string
		ipv4Result *models.PrivateIP
		ipv6Result *models.PrivateIP
	)
	for _, privateIP := range ips {
		family := ccev2.IPFamilyByIP(privateIP.PrivateIPAddress)
		if family == ccev2.IPv4Family {
			ipv4Result = &models.PrivateIP{
				PrivateIPAddress: privateIP.PrivateIPAddress,
				SubnetID:         privateIP.SubnetID,
				Primary:          privateIP.Primary,
			}
		} else if family == ccev2.IPv6Family {
			ipv6Result = &models.PrivateIP{
				PrivateIPAddress: privateIP.PrivateIPAddress,
				SubnetID:         privateIP.SubnetID,
				Primary:          privateIP.Primary,
			}
		}
	}
	// get eni
	action, err := n.__prepareIPAllocation(scopedLog, false)
	if err != nil {
		return
	}
	if action.AvailableForAllocationIPv4 == 0 && action.AvailableForAllocationIPv6 == 0 {
		if action.AvailableInterfaces == 0 {
			return "", fmt.Errorf("no available ip for allocation on node %s", n.k8sObj.Name)
		}
		_, eniID, err = n.CreateInterface(ctx, action, scopedLog)
		if err != nil {
			return "", fmt.Errorf("create interface failed: %v", err)
		}
		return "", fmt.Errorf("no available eni for allocation on node %s, try to create new eni %s", n.k8sObj.Name, eniID)
	}
	eniID = action.InterfaceID
	scopedLog = scopedLog.WithField("eni", eniID)
	scopedLog.Debug("prepare allocate ip cross subnet for eni")

	// check ip conflict
	// should to delete ip from the old eni
	isLocalIP, err := n.rleaseOldIP(ctx, scopedLog, ips, namespace, name, func(ctx context.Context, scopedLog *logrus.Entry, eniID string, toReleaseIPs []string) error {
		scopedLog.WithField("oldENI", eniID).WithField("toReleaseIPs", toReleaseIPs)
		err = n.manager.bceclient.BatchDeletePrivateIP(ctx, toReleaseIPs, eniID, false)
		if err != nil {
			scopedLog.Warnf("release ip %s from eni %s failed: %v", toReleaseIPs, eniID, err)
		} else {
			scopedLog.Info("release ip from old eni success")
		}
		return err
	})
	if err != nil {
		return
	}
	if isLocalIP {
		enis, _ := watchers.ENIClient.GetByIP(ipv4Result.PrivateIPAddress)
		eniID = enis[0].Name
		scopedLog.Infof("ip %s is local ip, directly reusable", ips[0].PrivateIPAddress)
		return
	}

	defer func() {
		// to roll back ip
		if err != nil {
			var ipsToRelease []string
			if ipv4Result != nil {
				ipsToRelease = append(ipsToRelease, ipv4Result.PrivateIPAddress)
			}
			if ipv6Result != nil {
				ipsToRelease = append(ipsToRelease, ipv6Result.PrivateIPAddress)
			}
			if len(ipsToRelease) > 0 {
				err2 := n.manager.bceclient.BatchDeletePrivateIP(ctx, ipsToRelease, eniID, true)
				if err2 != nil {
					scopedLog.WithError(err2).Error("release ip failed")
				}
			}
		}
	}()

	// allocate ipv4
	ipstr, err = n.manager.bceclient.BatchAddPrivateIpCrossSubnet(ctx, eniID, ipv4Result.SubnetID, []string{ipv4Result.PrivateIPAddress}, 1, false)
	if err != nil {
		ipv4Result = nil
		scopedLog.WithError(err).Error("allocate ip cross subnet failed")
		return
	}
	if len(ipstr) != 1 {
		ipv4Result = nil
		err = fmt.Errorf("allocate ip cross subnet failed, ipstr len %d", len(ipstr))
		scopedLog.WithError(err).Error("allocate ip cross subnet failed")
		return
	}
	scopedLog.WithField("ipv4", ipstr).Debug("allocate ipv4 cross subnet success")

	if operatorOption.Config.EnableIPv6 && ipv6Result != nil {
		ipstr, err = n.manager.bceclient.BatchAddPrivateIpCrossSubnet(ctx, eniID, ipv6Result.SubnetID, []string{ipv6Result.PrivateIPAddress}, 1, true)
		if n.manager.HandlerVPCError(scopedLog, err, ipv6Result.SubnetID) != nil {
			scopedLog.WithError(err).Error("allocate ipv6 cross subnet failed")
			return
		}
		if len(ipstr) != 1 {
			err = fmt.Errorf("allocate ipv6 cross subnet failed, ipstr len %d", len(ipstr))
			scopedLog.WithError(err).Error("allocate ipv6 cross subnet failed")
			return
		}
		scopedLog.WithField("ipv6", ipstr).Debug("allocate ipv6 cross subnet success")
	}
	scopedLog.Debug("update eni success")
	return

}

var _ realNodeInf = &bccNetworkResourceSet{}
