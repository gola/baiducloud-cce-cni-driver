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

package nodediscovery

import (
	"context"
	"fmt"
	"time"

	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/bce/agent"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/datapath"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/os"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"

	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/cidr"
	cnitypes "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/cni/types"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/defaults"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/ipam"
	ipamOption "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/ipam/option"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s"
	ccev2 "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s/apis/cce.baidubce.com/v2"
	ccev2alpha1 "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s/apis/cce.baidubce.com/v2alpha1"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/lock"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/logging"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/logging/logfields"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/mtu"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/node"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/node/addressing"
	nodemanager "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/node/manager"
	nodeTypes "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/node/types"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/option"
	k8sTypes "k8s.io/api/core/v1"
)

const (
	// AutoCIDR indicates that a CIDR should be allocated
	AutoCIDR = "auto"

	nodeDiscoverySubsys = "nodediscovery"
	maxRetryCount       = 10
)

var log = logging.DefaultLogger.WithField(logfields.LogSubsys, nodeDiscoverySubsys)

type k8sNodeGetter interface {
	GetK8sNode(ctx context.Context, nodeName string) (*corev1.Node, error)
}

type nrcsNodeGetter interface {
	GetNodeNrcs(nodeName string) (*ccev2alpha1.NetResourceConfigSet, error)
}

// NodeDiscovery represents a node discovery action
type NodeDiscovery struct {
	Manager               *nodemanager.Manager
	LocalConfig           datapath.LocalNodeConfiguration
	Registered            chan struct{}
	localStateInitialized chan struct{}
	NetConf               *cnitypes.NetConf
	k8sNodeGetter         k8sNodeGetter
	nrcsNodeGetter        nrcsNodeGetter
	localNodeLock         lock.Mutex
	localNode             nodeTypes.Node
	eventRecorder         record.EventRecorder
}

func enableLocalNodeRoute() bool {
	return option.Config.EnableLocalNodeRoute &&
		option.Config.IPAM != ipamOption.IPAMVpcEni
}

// NewNodeDiscovery returns a pointer to new node discovery object
func NewNodeDiscovery(manager *nodemanager.Manager, mtuConfig mtu.Configuration, netConf *cnitypes.NetConf) *NodeDiscovery {
	auxPrefixes := []*cidr.CIDR{}
	return &NodeDiscovery{
		Manager: manager,
		LocalConfig: datapath.LocalNodeConfiguration{
			MtuConfig:             mtuConfig,
			UseSingleClusterRoute: option.Config.UseSingleClusterRoute,
			EnableIPv4:            option.Config.EnableIPv4,
			EnableIPv6:            option.Config.EnableIPv6,
			// Ethernet NodeDiscovery is do not enable for RDMA, RDMA is only enabled in RdmaDiscovery
			EnableRDMA:              false,
			EnableAutoDirectRouting: option.Config.EnableAutoDirectRouting,
			EnableLocalNodeRoute:    enableLocalNodeRoute(),
			AuxiliaryPrefixes:       auxPrefixes,
			IPv4PodSubnets:          option.Config.IPv4PodSubnets,
			IPv6PodSubnets:          option.Config.IPv6PodSubnets,
		},
		localNode:             nodeTypes.Node{},
		Registered:            make(chan struct{}),
		localStateInitialized: make(chan struct{}),
		NetConf:               netConf,
		eventRecorder:         k8s.EventBroadcaster().NewRecorder(scheme.Scheme, corev1.EventSource{Component: nodeDiscoverySubsys}),
	}
}

// NewNodeDiscovery returns a pointer to new node discovery object
func NewRdmaDiscovery(manager *nodemanager.Manager, mtuConfig mtu.Configuration, netConf *cnitypes.NetConf) *RdmaDiscovery {
	return &RdmaDiscovery{NewNodeDiscovery(manager, mtuConfig, netConf)}
}

// StartDiscovery start configures the local node and starts node discovery. This is called on
// agent startup to configure the local node based on the configuration options
// passed to the agent. nodeName is the name to be used in the local agent.
func (n *NodeDiscovery) StartDiscovery() {
	n.localNodeLock.Lock()
	defer n.localNodeLock.Unlock()

	n.fillLocalNode()

	go func() {
		log.WithFields(
			logrus.Fields{
				logfields.Node: n.localNode,
			}).Info("Adding local node to cluster")
		close(n.Registered)
	}()

	go func() {
		select {
		case <-n.Registered:
		case <-time.After(defaults.NodeInitTimeout):
			log.Fatalf("Unable to initialize local node due to timeout")
		}
	}()

	n.Manager.NodeUpdated(n.localNode)
	close(n.localStateInitialized)

	n.updateLocalNode()
}

// WaitForLocalNodeInit blocks until StartDiscovery() has been called.  This is used to block until
// Node's local IP addresses have been allocated, see https://github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pull/14299
// and https://github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pull/14670.
func (n *NodeDiscovery) WaitForLocalNodeInit() {
	<-n.localStateInitialized
}

func (n *NodeDiscovery) NodeDeleted(node nodeTypes.Node) {
	n.Manager.NodeDeleted(node)
}

func (n *NodeDiscovery) NodeUpdated(node nodeTypes.Node) {
	n.Manager.NodeUpdated(node)
}

func (n *NodeDiscovery) ClusterSizeDependantInterval(baseInterval time.Duration) time.Duration {
	return n.Manager.ClusterSizeDependantInterval(baseInterval)
}

func (n *NodeDiscovery) fillLocalNode() {
	n.localNode.Name = nodeTypes.GetName()
	n.localNode.Cluster = option.Config.ClusterID
	n.localNode.IPAddresses = []nodeTypes.Address{}
	n.localNode.IPv4AllocCIDR = node.GetIPv4AllocRange()
	n.localNode.IPv6AllocCIDR = node.GetIPv6AllocRange()
	n.localNode.ClusterID = option.Config.ClusterID
	n.localNode.Labels = node.GetLabels()

	if node.GetK8sExternalIPv4() != nil {
		n.localNode.IPAddresses = append(n.localNode.IPAddresses, nodeTypes.Address{
			Type: addressing.NodeExternalIP,
			IP:   node.GetK8sExternalIPv4(),
		})
	}

	if node.GetIPv4() != nil {
		n.localNode.IPAddresses = append(n.localNode.IPAddresses, nodeTypes.Address{
			Type: addressing.NodeInternalIP,
			IP:   node.GetIPv4(),
		})
	}

	if node.GetIPv6() != nil {
		n.localNode.IPAddresses = append(n.localNode.IPAddresses, nodeTypes.Address{
			Type: addressing.NodeInternalIP,
			IP:   node.GetIPv6(),
		})
	}

	if node.GetInternalIPv4Router() != nil {
		n.localNode.IPAddresses = append(n.localNode.IPAddresses, nodeTypes.Address{
			Type: addressing.NodeCCEInternalIP,
			IP:   node.GetInternalIPv4Router(),
		})
	}

	if node.GetIPv6Router() != nil {
		n.localNode.IPAddresses = append(n.localNode.IPAddresses, nodeTypes.Address{
			Type: addressing.NodeCCEInternalIP,
			IP:   node.GetIPv6Router(),
		})
	}

	if node.GetK8sExternalIPv6() != nil {
		n.localNode.IPAddresses = append(n.localNode.IPAddresses, nodeTypes.Address{
			Type: addressing.NodeExternalIP,
			IP:   node.GetK8sExternalIPv6(),
		})
	}
}

func (n *NodeDiscovery) updateLocalNode() {
	// we should init the os distribution before we do anything
	release, err := os.NewOSDistribution()
	if err != nil {
		log.WithError(err).Fatal("Unable to detect OS distribution")
	}
	_ = release.HostOS().DisableAndMonitorMacPersistant()

	if k8s.IsEnabled() {
		// CRD IPAM endpoint restoration depends on the completion of this
		// to avoid custom resource update conflicts.
		n.UpdateNetResourceSetResource()
	}
}

// UpdateLocalNode syncs the internal localNode object with the actual state of
// the local node and publishes the corresponding updated KV store entry and/or
// NetResourceSet object
func (n *NodeDiscovery) UpdateLocalNode() {
	n.localNodeLock.Lock()
	defer n.localNodeLock.Unlock()

	n.fillLocalNode()
	n.updateLocalNode()
}

// Close shuts down the node discovery engine
func (n *NodeDiscovery) Close() {
	n.Manager.Close()
}

// UpdateNetResourceSetResource updates the NetResourceSet resource representing the
// local node
func (n *NodeDiscovery) UpdateNetResourceSetResource() {
	if !option.Config.AutoCreateNetResourceSetResource {
		return
	}

	log.WithField(logfields.Node, nodeTypes.GetName()).Info("Creating or updating NetResourceSet resource")

	cceClient := k8s.CCEClient()

	performGet := true
	var nodeResource *ccev2.NetResourceSet
	for retryCount := 0; retryCount < maxRetryCount; retryCount++ {
		performUpdate := true
		if performGet {
			var err error
			nodeResource, err = cceClient.CceV2().NetResourceSets().Get(context.TODO(), nodeTypes.GetName(), metav1.GetOptions{})
			if err != nil {
				performUpdate = false
				nodeResource = &ccev2.NetResourceSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:   nodeTypes.GetName(),
						Labels: map[string]string{},
					},
				}
			} else {
				performGet = false
			}
		}

		if err := n.mutateNodeResource(nodeResource); err != nil {
			log.WithError(err).WithField("retryCount", retryCount).Warning("Unable to mutate nodeResource")
			continue
		}

		// if we retry after this point, is due to a conflict. We will do
		// a new GET  to ensure we have the latest information before
		// updating.
		performGet = true
		if performUpdate {
			if _, err := cceClient.CceV2().NetResourceSets().Update(context.TODO(), nodeResource, metav1.UpdateOptions{}); err != nil {
				if k8serrors.IsConflict(err) {
					log.WithError(err).Warn("Unable to update NetResourceSet resource, will retry")
					continue
				}
				log.WithError(err).Fatal("Unable to update NetResourceSet resource")
			} else {
				return
			}
		} else {
			if _, err := cceClient.CceV2().NetResourceSets().Create(context.TODO(), nodeResource, metav1.CreateOptions{}); err != nil {
				if k8serrors.IsConflict(err) {
					log.WithError(err).Warn("Unable to create NetResourceSet resource, will retry")
					continue
				}
				log.WithError(err).Fatal("Unable to create NetResourceSet resource")
			} else {
				log.Info("Successfully created NetResourceSet resource")
				return
			}
		}
	}
	log.Fatal("Could not create or update NetResourceSet resource, despite retries")
}

func (n *NodeDiscovery) mutateNodeResource(nodeResource *ccev2.NetResourceSet) error {
	// reset NetworkresourceSet.Spec.Addresses to avoid stale data
	nodeResource.Spec.Addresses = []ccev2.NodeAddress{}

	// If we are unable to fetch the K8s Node resource and the NetResourceSet does
	// not have an OwnerReference set, then somehow we are running in an
	// environment where only the NetResourceSet exists. Do not proceed as this is
	// unexpected.
	//
	// Note that we can rely on the OwnerReference to be set on the NetResourceSet
	// as this was added in sufficiently earlier versions of CCE (v1.6).
	// Source:
	// https://github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/commit/5c365f2c6d7930dcda0b8f0d5e6b826a64022a4f
	k8sNode, err := n.k8sNodeGetter.GetK8sNode(
		context.TODO(),
		nodeTypes.GetName(),
	)
	switch {
	case err != nil && k8serrors.IsNotFound(err) && len(nodeResource.ObjectMeta.OwnerReferences) == 0:
		log.WithError(err).WithField(
			logfields.NodeName, nodeTypes.GetName(),
		).Fatal(
			"Kubernetes Node resource does not exist, setting OwnerReference on " +
				"NetResourceSet is impossible. This is unexpected. Please investigate " +
				"why CCE is running on a Node that supposedly does not exist " +
				"according to Kubernetes.",
		)
	case err != nil && !k8serrors.IsNotFound(err):
		return fmt.Errorf("failed to fetch Kubernetes Node resource: %w", err)
	}

	nodeResource.ObjectMeta.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "Node",
		Name:       nodeTypes.GetName(),
		UID:        k8sNode.UID,
	}}

	// Get the addresses from k8s node and add them as part of CCE Node.
	// CCE Node should contain all addresses from k8s.
	k8sNodeParsed := k8s.ParseNode(k8sNode)

	// overwrite the labels from k8s node
	if nodeResource.ObjectMeta.Labels == nil {
		nodeResource.ObjectMeta.Labels = map[string]string{}
	}
	for key, v := range k8sNodeParsed.Labels {
		nodeResource.ObjectMeta.Labels[key] = v
	}
	nodeResource.ObjectMeta.Labels[k8s.LabelNodeName] = nodeTypes.GetName()

	if nodeResource.ObjectMeta.Annotations == nil {
		nodeResource.ObjectMeta.Annotations = map[string]string{}
	}
	for key, v := range k8sNodeParsed.Annotations {
		nodeResource.ObjectMeta.Annotations[key] = v
	}

	for _, k8sAddress := range k8sNodeParsed.IPAddresses {
		k8sAddressStr := k8sAddress.IP.String()
		nodeResource.Spec.Addresses = append(nodeResource.Spec.Addresses, ccev2.NodeAddress{
			Type: k8sAddress.Type,
			IP:   k8sAddressStr,
		})
	}

	for _, address := range n.localNode.IPAddresses {
		netResourceSetAddress := address.IP.String()
		var found bool
		for _, nodeResourceAddress := range nodeResource.Spec.Addresses {
			if nodeResourceAddress.IP == netResourceSetAddress {
				found = true
				break
			}
		}
		if !found {
			nodeResource.Spec.Addresses = append(nodeResource.Spec.Addresses, ccev2.NodeAddress{
				Type: address.Type,
				IP:   netResourceSetAddress,
			})
		}
	}

	instanceID, err := agent.GetInstanceID()
	if err != nil || instanceID == "" {
		n.eventRecorder.Eventf(k8sNode, k8sTypes.EventTypeWarning, "MetaAPIError01", "failed to get instance id: %v", err)
		log.WithError(err).Fatal("get instance id fail")
	}
	nodeResource.Spec.InstanceID = instanceID

	nrcs, err := n.nrcsNodeGetter.GetNodeNrcs(nodeTypes.GetName())
	if err != nil {
		log.WithError(err).Warning("get node NRCs failed")
	}
	switch option.Config.IPAM {
	case ipamOption.IPAMClusterPool:
	case ipamOption.IPAMClusterPoolV2, ipamOption.IPAMVpcRoute:
		if c := n.NetConf; c != nil {
			nodeResource.Spec.IPAM.PodCIDRAllocationThreshold = c.IPAM.PodCIDRAllocationThreshold
			nodeResource.Spec.IPAM.PodCIDRReleaseThreshold = c.IPAM.PodCIDRReleaseThreshold
		}
	case ipamOption.IPAMVpcEni:
		n.refreshVpcENIConfiguration(nodeResource, nrcs, k8sNode)

	case ipamOption.IPAMPrivateCloudBase:
		if c := n.NetConf; c != nil {
			if c.IPAM.MinAllocate != 0 {
				nodeResource.Spec.IPAM.MinAllocate = c.IPAM.MinAllocate
			}
			if c.IPAM.PreAllocate != 0 {
				nodeResource.Spec.IPAM.PreAllocate = c.IPAM.PreAllocate
			}
		}
		node.SetK8sNodeIP(k8sNodeParsed.GetIPByType(addressing.NodeInternalIP, false))
		node.SetIPv4(k8sNodeParsed.GetIPByType(addressing.NodeInternalIP, false))
		node.SetIPv4AllocRange(k8sNodeParsed.IPv4AllocCIDR)
		node.SetIPv6(k8sNodeParsed.GetIPByType(addressing.NodeInternalIP, true))
		node.SetIPv6NodeRange(k8sNodeParsed.IPv6AllocCIDR)

	// We want to keep the podCIDRs untouched in these IPAM modes because
	// the operator will verify if it can assign such podCIDRs.
	// If the user was running in non-IPAM Operator mode and then switched
	// to IPAM Operator, then it is possible that the previous cluster CIDR
	// from the old IPAM mode differs from the current cluster CIDR set in
	// the operator.
	// There is a chance that the operator won't be able to allocate these
	// podCIDRs, resulting in an error in the NetResourceSet status.
	default:
		nodeResource.Spec.IPAM.PodCIDRs = []string{}
		if cidr := node.GetIPv4AllocRange(); cidr != nil {
			nodeResource.Spec.IPAM.PodCIDRs = append(nodeResource.Spec.IPAM.PodCIDRs, cidr.String())
		}

		if cidr := node.GetIPv6AllocRange(); cidr != nil {
			nodeResource.Spec.IPAM.PodCIDRs = append(nodeResource.Spec.IPAM.PodCIDRs, cidr.String())
		}
	}

	return nil
}

// refreshVpcENIConfiguration update eni spec param
// over write the nrcs config
// only generate eni spec when it is not set
// if eni use mode was set on the label of local node, we use it override the eni spec
// reset eni spec when it is restart
// clean ippool config
// set max allocate ips to node default is 128
// update subnet and security group ids
// this update does not take effect on existing ENI
func (n *NodeDiscovery) refreshVpcENIConfiguration(nodeResource *ccev2.NetResourceSet, nrcs *ccev2alpha1.NetResourceConfigSet, k8sNode *k8sTypes.Node) {
	var (
		preAllocateENI       = option.Config.ENI.PreAllocateENI
		minAllocate          = option.Config.IPPoolMinAllocateIPs
		preAllocate          = option.Config.IPPoolPreAllocate
		maxAboveWatermark    = option.Config.IPPoolMaxAboveWatermark
		routeTableOffset     = option.Config.ENI.RouteTableOffset
		burstableMehrfachENI = option.Config.BurstableMehrfachENI

		subnetIDs                  = option.Config.ENI.SubnetIDs
		securityGroups             = option.Config.ENI.SecurityGroups
		enterpriseSecurityGroupIDs = option.Config.ENI.EnterpriseSecurityGroupList

		eniUseMode        string
		usePrimaryAddress bool
	)

	if mode, ok := nodeResource.Labels[k8s.LabelENIUseMode]; ok {
		eniUseMode = mode
	}

	if nrcs != nil {
		log.WithFields(
			logrus.Fields{
				"nrcs": nrcs.Name,
				"spec": logfields.Repr(nrcs.Spec),
			},
		)
		if nrcs.Spec.AgentConfig.IPPoolPreAllocateENI != nil {
			preAllocateENI = *nrcs.Spec.AgentConfig.IPPoolPreAllocateENI
		}
		if nrcs.Spec.AgentConfig.IPPoolMinAllocate != nil {
			minAllocate = *nrcs.Spec.AgentConfig.IPPoolMinAllocate
		}
		if nrcs.Spec.AgentConfig.IPPoolPreAllocate != nil {
			preAllocate = *nrcs.Spec.AgentConfig.IPPoolPreAllocate
		}
		if nrcs.Spec.AgentConfig.IPPoolMaxAboveWatermark != nil {
			maxAboveWatermark = *nrcs.Spec.AgentConfig.IPPoolMaxAboveWatermark
		}
		if nrcs.Spec.AgentConfig.RouteTableOffset != nil {
			routeTableOffset = *nrcs.Spec.AgentConfig.RouteTableOffset
		}
		if nrcs.Spec.AgentConfig.BurstableMehrfachENI != nil {
			burstableMehrfachENI = *nrcs.Spec.AgentConfig.BurstableMehrfachENI
		}
		if nrcs.Spec.AgentConfig.EniUseMode != nil {
			eniUseMode = *nrcs.Spec.AgentConfig.EniUseMode
		}
		if nrcs.Spec.AgentConfig.ExtCniPlugins != nil {
			option.Config.ExtCNIPluginsList = nrcs.Spec.AgentConfig.ExtCniPlugins
		}

		if len(nrcs.Spec.AgentConfig.EniSubnetIDs) > 0 {
			subnetIDs = nrcs.Spec.AgentConfig.EniSubnetIDs
		}
		if len(nrcs.Spec.AgentConfig.EniSecurityGroupIds) > 0 {
			securityGroups = nrcs.Spec.AgentConfig.EniSecurityGroupIds
		}
		if len(nrcs.Spec.AgentConfig.EniEnterpriseSecurityGroupIds) > 0 {
			enterpriseSecurityGroupIDs = nrcs.Spec.AgentConfig.EniEnterpriseSecurityGroupIds
		}
		if nrcs.Spec.AgentConfig.UsePrimaryAddress != nil {
			usePrimaryAddress = *nrcs.Spec.AgentConfig.UsePrimaryAddress
		}
	}

	// create new nrs if it is not set
	if nodeResource.Spec.ENI == nil {
		eni, err := agent.GenerateENISpec()
		if err != nil {
			n.eventRecorder.Eventf(k8sNode, k8sTypes.EventTypeWarning, "MetaAPIError02", "generate eni metadata error: %v", err)
			log.WithError(err).Fatal("generate ENI spec fail")
		}
		nodeResource.Spec.ENI = eni

		nodeResource.Spec.ENI.UseMode = eniUseMode
		if nodeResource.Spec.ENI.UseMode == string(ccev2.ENIUseModeSecondaryIP) {
			if option.Config.ENI != nil {
				nodeResource.Spec.ENI.InstallSourceBasedRouting = option.Config.ENI.InstallSourceBasedRouting
			}
		}
		nodeResource.Spec.ENI.UsePrimaryAddress = &usePrimaryAddress
		if routeTableOffset > 0 {
			nodeResource.Spec.ENI.RouteTableOffset = routeTableOffset
		}
	}

	// update old nrs
	if nodeResource.Spec.ENI.UseMode != string(ccev2.ENIUseModePrimaryIP) {
		nodeResource.Spec.ENI.BurstableMehrfachENI = burstableMehrfachENI
		nodeResource.Spec.ENI.PreAllocateENI = preAllocateENI

		if burstableMehrfachENI == 0 {
			nodeResource.Spec.IPAM.MinAllocate = minAllocate
			nodeResource.Spec.IPAM.PreAllocate = preAllocate
			nodeResource.Spec.IPAM.MaxAboveWatermark = maxAboveWatermark
		}

		podsNum := k8sNode.Status.Capacity[k8sTypes.ResourcePods]
		if nums, ok := podsNum.AsInt64(); ok {
			nodeResource.Spec.IPAM.MaxAllocate = int(nums)
			log.Infof("set max allocate ips %d to nrs", nodeResource.Spec.IPAM.MaxAllocate)
		} else {
			log.Warnf("failed to get pods number from node %s, will use default 128", k8sNode.Name)
			nodeResource.Spec.IPAM.MaxAllocate = 128
		}
	}
	nodeResource.Spec.ENI.SecurityGroups = securityGroups
	nodeResource.Spec.ENI.EnterpriseSecurityGroupList = enterpriseSecurityGroupIDs

	// do not leak subnet ids which used by eni
	nodeResource.Spec.ENI.SubnetIDs = subnetIDs
	if len(nodeResource.Status.ENIs) > 0 {
		sbnSet := sets.NewString(nodeResource.Spec.ENI.SubnetIDs...)
		for _, eni := range nodeResource.Status.ENIs {
			sbnSet.Insert(eni.SubnetID)
		}
		nodeResource.Spec.ENI.SubnetIDs = sbnSet.List()
	}
}

func (n *NodeDiscovery) RegisterK8sNodeGetter(k8sNodeGetter k8sNodeGetter) {
	n.k8sNodeGetter = k8sNodeGetter
}

func (n *NodeDiscovery) RegisterNrcsNodeGetter(nrcsNodeGetter nrcsNodeGetter) {
	n.nrcsNodeGetter = nrcsNodeGetter
}

// LocalAllocCIDRsUpdated informs the agent that the local allocation CIDRs have
// changed. This will inform the datapath node manager to update the local node
// routes accordingly.
// The first CIDR in ipv[46]AllocCIDRs is presumed to be the primary CIDR: This
// CIDR remains assigned to the local node and may not be switched out or be
// removed.
func (n *NodeDiscovery) LocalAllocCIDRsUpdated(ipv4AllocCIDRs, ipv6AllocCIDRs []*cidr.CIDR) {
	n.localNodeLock.Lock()
	defer n.localNodeLock.Unlock()

	if option.Config.EnableIPv4 && len(ipv4AllocCIDRs) > 0 {
		ipv4PrimaryCIDR, ipv4SecondaryCIDRs := splitAllocCIDRs(ipv4AllocCIDRs)
		validatePrimaryCIDR(n.localNode.IPv4AllocCIDR, ipv4PrimaryCIDR, ipam.IPv4)
		n.localNode.IPv4AllocCIDR = ipv4PrimaryCIDR
		n.localNode.IPv4SecondaryAllocCIDRs = ipv4SecondaryCIDRs
	}

	if option.Config.EnableIPv6 && len(ipv6AllocCIDRs) > 0 {
		ipv6PrimaryCIDR, ipv6SecondaryCIDRs := splitAllocCIDRs(ipv6AllocCIDRs)
		validatePrimaryCIDR(n.localNode.IPv6AllocCIDR, ipv6PrimaryCIDR, ipam.IPv6)
		n.localNode.IPv6AllocCIDR = ipv6PrimaryCIDR
		n.localNode.IPv6SecondaryAllocCIDRs = ipv6SecondaryCIDRs
	}

	n.Manager.NodeUpdated(n.localNode)
}

func (n *NodeDiscovery) ResourceType() string {
	return ccev2.NetResourceSetEventHandlerTypeEth
}

func splitAllocCIDRs(allocCIDRs []*cidr.CIDR) (primaryCIDR *cidr.CIDR, secondaryCIDRS []*cidr.CIDR) {
	secondaryCIDRS = make([]*cidr.CIDR, 0, len(allocCIDRs)-1)
	for i, allocCIDR := range allocCIDRs {
		if i == 0 {
			primaryCIDR = allocCIDR
		} else {
			secondaryCIDRS = append(secondaryCIDRS, allocCIDR)
		}
	}

	return primaryCIDR, secondaryCIDRS
}

func validatePrimaryCIDR(oldCIDR, newCIDR *cidr.CIDR, family ipam.Family) {
	if oldCIDR != nil && !oldCIDR.Equal(newCIDR) {
		newCIDRStr := "<nil>"
		if newCIDR != nil {
			newCIDRStr = newCIDR.String()
		}

		log.WithFields(logrus.Fields{
			logfields.OldCIDR: oldCIDR.String(),
			logfields.NewCIDR: newCIDRStr,
			logfields.Family:  family,
		}).Warn("Detected change of primary pod allocation CIDR. Agent restart required.")
	}
}
