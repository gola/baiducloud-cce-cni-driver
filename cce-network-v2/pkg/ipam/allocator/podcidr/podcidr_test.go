//go:build !privileged_tests

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

package podcidr

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	. "gopkg.in/check.v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/operator/watchers"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/checker"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/controller"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/ipam"
	ipamOption "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/ipam/option"
	ipamTypes "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/ipam/types"
	v2 "github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/k8s/apis/cce.baidubce.com/v2"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/option"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/pkg/trigger"
	"github.com/baidubce/baiducloud-cce-cni-driver/cce-network-v2/test/mock/ccemock"
	"github.com/stretchr/testify/assert"
)

func Test(t *testing.T) {
	TestingT(t)
}

type PodCIDRSuite struct{}

var _ = Suite(&PodCIDRSuite{})

func mustNewCIDRs(cidrs ...string) []*net.IPNet {
	ipnets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		ipnets = append(ipnets, ipNet)
	}
	return ipnets
}

func mustNewTrigger(f func(), minInterval time.Duration) *trigger.Trigger {
	t, err := trigger.NewTrigger(trigger.Parameters{
		MinInterval: minInterval,
		TriggerFunc: func(reasons []string) {
			f()
		},
		Name: "",
	})
	if err != nil {
		panic(err)
	}
	return t
}

var defaultIPAMModes = []string{ipamOption.IPAMClusterPool}

func runWithIPAMModes(ipamModes []string, testFunc func(mode string)) {
	oldIPAMMode := option.Config.IPAM
	defer func() {
		option.Config.IPAM = oldIPAMMode
	}()
	for _, ipamMode := range ipamModes {
		option.Config.IPAM = ipamMode
		testFunc(ipamMode)
	}
}

type mockCIDRAllocator struct {
	OnOccupy       func(cidr *net.IPNet) error
	OnAllocateNext func() (*net.IPNet, error)
	OnRelease      func(cidr *net.IPNet) error
	OnIsAllocated  func(cidr *net.IPNet) (bool, error)
	OnIsIPv6       func() bool
	OnIsFull       func() bool
	OnInRange      func(cidr *net.IPNet) bool
}

func (d *mockCIDRAllocator) String() string {
	return "clusterCIDR: 10.0.0.0/24, nodeMask: 24"
}

func (d *mockCIDRAllocator) Occupy(cidr *net.IPNet) error {
	if d.OnOccupy != nil {
		return d.OnOccupy(cidr)
	}
	panic("d.Occupy should not have been called!")
}

func (d *mockCIDRAllocator) AllocateNext() (*net.IPNet, error) {
	if d.OnAllocateNext != nil {
		return d.OnAllocateNext()
	}
	panic("d.AllocateNext should not have been called!")
}

func (d *mockCIDRAllocator) Release(cidr *net.IPNet) error {
	if d.OnRelease != nil {
		return d.OnRelease(cidr)
	}
	panic("d.Release should not have been called!")
}

func (d *mockCIDRAllocator) IsAllocated(cidr *net.IPNet) (bool, error) {
	if d.OnIsAllocated != nil {
		return d.OnIsAllocated(cidr)
	}
	panic("d.IsAllocated should not have been called!")
}

func (d *mockCIDRAllocator) IsIPv6() bool {
	if d.OnIsIPv6 != nil {
		return d.OnIsIPv6()
	}
	panic("d.IsIPv6 should not have been called!")
}

func (d *mockCIDRAllocator) IsFull() bool {
	if d.OnIsFull != nil {
		return d.OnIsFull()
	}
	panic("d.IsFull should not have been called!")
}

func (d *mockCIDRAllocator) InRange(cidr *net.IPNet) bool {
	if d.OnInRange != nil {
		return d.OnInRange(cidr)
	}
	panic("d.InRange should not have been called!")
}

type k8sNodeMock struct {
	OnUpdate       func(oldNode, newNode *v2.NetResourceSet) (*v2.NetResourceSet, error)
	OnUpdateStatus func(oldNode, newNode *v2.NetResourceSet) (*v2.NetResourceSet, error)
	OnGet          func(node string) (*v2.NetResourceSet, error)
	OnCreate       func(n *v2.NetResourceSet) (*v2.NetResourceSet, error)
	OnDelete       func(nodeName string) error
}

func (k *k8sNodeMock) Update(origNode, node *v2.NetResourceSet) (*v2.NetResourceSet, error) {
	if k.OnUpdate != nil {
		return k.OnUpdate(origNode, node)
	}
	panic("d.Update should not be called!")
}

func (k *k8sNodeMock) UpdateStatus(origNode, node *v2.NetResourceSet) (*v2.NetResourceSet, error) {
	if k.OnUpdateStatus != nil {
		return k.OnUpdateStatus(origNode, node)
	}
	panic("d.UpdateStatus should not be called!")
}

func (k *k8sNodeMock) Get(node string) (*v2.NetResourceSet, error) {
	if k.OnGet != nil {
		return k.OnGet(node)
	}
	panic("d.Get should not be called!")
}

func (k *k8sNodeMock) Create(n *v2.NetResourceSet) (*v2.NetResourceSet, error) {
	if k.OnCreate != nil {
		return k.OnCreate(n)
	}
	panic("d.Create should not be called!")
}

func (s *PodCIDRSuite) TestNodesPodCIDRManager_Create(c *C) {
	var reSyncCalls int32
	type fields struct {
		k8sReSyncController  *controller.Manager
		k8sReSync            *trigger.Trigger
		canAllocateNodes     bool
		v4ClusterCIDRs       []CIDRAllocator
		v6ClusterCIDRs       []CIDRAllocator
		nodes                map[string]*nodeCIDRs
		netResourceSetsToK8s map[string]*netResourceSetK8sOp
	}
	type args struct {
		node *v2.NetResourceSet
	}
	tests := []struct {
		testSetup   func() *fields
		testPostRun func(fields *fields)
		name        string
		fields      *fields
		args        args
		want        error
	}{
		{
			name: "test-1 - should allocate a v4 addr",
			want: nil,
			testSetup: func() *fields {
				atomic.StoreInt32(&reSyncCalls, 0)
				return &fields{
					canAllocateNodes: true,
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnAllocateNext: func() (ipNet *net.IPNet, err error) {
								return mustNewCIDRs("10.10.0.0/24")[0], nil
							},
							OnIsFull: func() bool {
								return false
							},
						},
					},
					nodes:                map[string]*nodeCIDRs{},
					netResourceSetsToK8s: map[string]*netResourceSetK8sOp{},
					k8sReSync: mustNewTrigger(func() {
						atomic.AddInt32(&reSyncCalls, 1)
						return
					}, time.Millisecond),
				}
			},
			testPostRun: func(fields *fields) {
				time.Sleep(2 * time.Millisecond)
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
					},
				})
				c.Assert(fields.netResourceSetsToK8s, checker.DeepEquals, map[string]*netResourceSetK8sOp{
					"node-1": {
						netResourceSet: &v2.NetResourceSet{
							ObjectMeta: v1.ObjectMeta{
								Name: "node-1",
							},
							Spec: v2.NetResourceSpec{
								IPAM: ipamTypes.IPAMSpec{
									PodCIDRs: []string{
										"10.10.0.0/24",
									},
								},
							},
						},
						op: k8sOpCreate,
					},
				})
				c.Assert(atomic.LoadInt32(&reSyncCalls), Equals, int32(1))
			},
			args: args{
				node: &v2.NetResourceSet{
					ObjectMeta: v1.ObjectMeta{
						Name: "node-1",
					},
				},
			},
		},
		{
			name: "test-2 - failed to allocate a v4 addr",
			want: nil,
			testSetup: func() *fields {
				atomic.StoreInt32(&reSyncCalls, 0)
				return &fields{
					canAllocateNodes: true,
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnAllocateNext: func() (ipNet *net.IPNet, err error) {
								return nil, fmt.Errorf("Allocator full!")
							},
							OnIsFull: func() bool {
								return false
							},
						},
					},
					nodes:                map[string]*nodeCIDRs{},
					netResourceSetsToK8s: map[string]*netResourceSetK8sOp{},
					k8sReSync: mustNewTrigger(func() {
						atomic.AddInt32(&reSyncCalls, 1)
						return
					}, time.Millisecond),
				}
			},
			testPostRun: func(fields *fields) {
				time.Sleep(2 * time.Millisecond)
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{})
				c.Assert(fields.netResourceSetsToK8s, checker.DeepEquals, map[string]*netResourceSetK8sOp{
					"node-1": {
						netResourceSet: &v2.NetResourceSet{
							ObjectMeta: v1.ObjectMeta{
								Name: "node-1",
							},
							Status: v2.NetResourceStatus{
								IPAM: ipamTypes.IPAMStatus{
									OperatorStatus: ipamTypes.OperatorStatus{
										Error: "Allocator full!",
									},
								},
							},
						},
						op: k8sOpCreate,
					},
				})
				c.Assert(atomic.LoadInt32(&reSyncCalls), Equals, int32(1))
			},
			args: args{
				node: &v2.NetResourceSet{
					ObjectMeta: v1.ObjectMeta{
						Name: "node-1",
					},
				},
			},
		},
		{
			name: "test-3 - node is already allocated with the requested pod CIDRs",
			want: nil,
			testSetup: func() *fields {
				return &fields{
					canAllocateNodes: true,
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{},
					},
					nodes: map[string]*nodeCIDRs{
						"node-1": {
							v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						},
					},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
					},
				})
			},
			args: args{
				node: &v2.NetResourceSet{
					ObjectMeta: v1.ObjectMeta{
						Name: "node-1",
					},
					Spec: v2.NetResourceSpec{
						IPAM: ipamTypes.IPAMSpec{
							PodCIDRs: []string{
								"10.10.0.0/24",
							},
						},
					},
				},
			},
		},
		{
			name: "test-4 - node is requesting pod CIDRs, it's already locally allocated but the spec is not updated",
			want: nil,
			testSetup: func() *fields {
				atomic.StoreInt32(&reSyncCalls, 0)
				return &fields{
					canAllocateNodes: true,
					nodes: map[string]*nodeCIDRs{
						"node-1": {
							v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						},
					},
					netResourceSetsToK8s: map[string]*netResourceSetK8sOp{},
					k8sReSync: mustNewTrigger(func() {
						atomic.AddInt32(&reSyncCalls, 1)
						return
					}, time.Millisecond),
				}
			},
			testPostRun: func(fields *fields) {
				time.Sleep(2 * time.Millisecond)
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
					},
				})
				c.Assert(fields.netResourceSetsToK8s, checker.DeepEquals, map[string]*netResourceSetK8sOp{
					"node-1": {
						netResourceSet: &v2.NetResourceSet{
							ObjectMeta: v1.ObjectMeta{
								Name: "node-1",
							},
							Spec: v2.NetResourceSpec{
								IPAM: ipamTypes.IPAMSpec{
									PodCIDRs: []string{
										"10.10.0.0/24",
									},
								},
							},
						},
						op: k8sOpCreate,
					},
				})
				c.Assert(atomic.LoadInt32(&reSyncCalls), Equals, int32(1))
			},
			args: args{
				node: &v2.NetResourceSet{
					ObjectMeta: v1.ObjectMeta{
						Name: "node-1",
					},
				},
			},
		},
		{
			name: "test-5 - node requires a new CIDR but the first allocator is full",
			want: nil,
			testSetup: func() *fields {
				atomic.StoreInt32(&reSyncCalls, 0)
				return &fields{
					canAllocateNodes: true,
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnIsFull: func() bool {
								return true
							},
						},
						&mockCIDRAllocator{
							OnAllocateNext: func() (ipNet *net.IPNet, err error) {
								return mustNewCIDRs("10.10.0.0/24")[0], nil
							},
							OnIsFull: func() bool {
								return false
							},
						},
					},
					nodes:                map[string]*nodeCIDRs{},
					netResourceSetsToK8s: map[string]*netResourceSetK8sOp{},
					k8sReSync: mustNewTrigger(func() {
						atomic.AddInt32(&reSyncCalls, 1)
						return
					}, time.Millisecond),
				}
			},
			testPostRun: func(fields *fields) {
				time.Sleep(2 * time.Millisecond)
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
					},
				})
				c.Assert(fields.netResourceSetsToK8s, checker.DeepEquals, map[string]*netResourceSetK8sOp{
					"node-1": {
						netResourceSet: &v2.NetResourceSet{
							ObjectMeta: v1.ObjectMeta{
								Name: "node-1",
							},
							Spec: v2.NetResourceSpec{
								IPAM: ipamTypes.IPAMSpec{
									PodCIDRs: []string{
										"10.10.0.0/24",
									},
								},
							},
						},
						op: k8sOpCreate,
					},
				})
				c.Assert(atomic.LoadInt32(&reSyncCalls), Equals, int32(1))
			},
			args: args{
				node: &v2.NetResourceSet{
					ObjectMeta: v1.ObjectMeta{
						Name: "node-1",
					},
				},
			},
		},
	}

	runWithIPAMModes(defaultIPAMModes, func(ipamMode string) {
		for _, tt := range tests {
			c.Logf("Running %q (ipam: %s)", tt.name, ipamMode)
			tt.fields = tt.testSetup()
			n := &NodesPodCIDRManager{
				k8sReSyncController:  tt.fields.k8sReSyncController,
				k8sReSync:            tt.fields.k8sReSync,
				canAllocatePodCIDRs:  tt.fields.canAllocateNodes,
				v4CIDRAllocators:     tt.fields.v4ClusterCIDRs,
				v6CIDRAllocators:     tt.fields.v6ClusterCIDRs,
				nodes:                tt.fields.nodes,
				netResourceSetsToK8s: tt.fields.netResourceSetsToK8s,
			}
			got := n.Create(tt.args.node)
			c.Assert(got, checker.Equals, tt.want, Commentf("Test Name: %s", tt.name))

			if tt.testPostRun != nil {
				tt.testPostRun(tt.fields)
			}
		}
	})
}

func (s *PodCIDRSuite) TestNodesPodCIDRManager_Delete(c *C) {
	var reSyncCalls int32
	type fields struct {
		k8sReSyncController  *controller.Manager
		k8sReSync            *trigger.Trigger
		canAllocateNodes     bool
		v4ClusterCIDRs       []CIDRAllocator
		v6ClusterCIDRs       []CIDRAllocator
		nodes                map[string]*nodeCIDRs
		netResourceSetsToK8s map[string]*netResourceSetK8sOp
	}
	type args struct {
		nodeName string
	}
	tests := []struct {
		testSetup   func() *fields
		testPostRun func(fields *fields)
		name        string
		fields      *fields
		args        args
	}{
		{
			name: "test-1 - should release the v4 CIDR",
			testSetup: func() *fields {
				atomic.StoreInt32(&reSyncCalls, 0)
				atomic.StoreInt32(&reSyncCalls, 0)
				return &fields{
					canAllocateNodes: true,
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnRelease: func(cidr *net.IPNet) error {
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								return nil
							},
							OnInRange: func(cidr *net.IPNet) bool {
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								return true
							},
						},
					},
					nodes: map[string]*nodeCIDRs{
						"node-1": {
							v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						},
					},
					netResourceSetsToK8s: map[string]*netResourceSetK8sOp{},
					k8sReSync: mustNewTrigger(func() {
						atomic.AddInt32(&reSyncCalls, 1)
						return
					}, time.Millisecond),
				}
			},
			testPostRun: func(fields *fields) {
				time.Sleep(2 * time.Millisecond)
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{})
				c.Assert(fields.netResourceSetsToK8s, checker.DeepEquals, map[string]*netResourceSetK8sOp{
					"node-1": {
						op: k8sOpDelete,
					},
				})
				c.Assert(atomic.LoadInt32(&reSyncCalls), Equals, int32(1))
			},
			args: args{
				nodeName: "node-1",
			},
		},
		{
			name: "test-2 - should be a no op since the node is not allocated",
			testSetup: func() *fields {
				atomic.StoreInt32(&reSyncCalls, 0)
				return &fields{
					canAllocateNodes:     true,
					netResourceSetsToK8s: map[string]*netResourceSetK8sOp{},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.netResourceSetsToK8s, checker.DeepEquals, map[string]*netResourceSetK8sOp{})
				c.Assert(atomic.LoadInt32(&reSyncCalls), Equals, int32(0))
			},
			args: args{
				nodeName: "node-1",
			},
		},
	}

	runWithIPAMModes(defaultIPAMModes, func(ipamMode string) {
		for _, tt := range tests {
			c.Logf("Running %q (ipam: %s)", tt.name, ipamMode)
			tt.fields = tt.testSetup()
			n := &NodesPodCIDRManager{
				k8sReSyncController:  tt.fields.k8sReSyncController,
				k8sReSync:            tt.fields.k8sReSync,
				canAllocatePodCIDRs:  tt.fields.canAllocateNodes,
				v4CIDRAllocators:     tt.fields.v4ClusterCIDRs,
				v6CIDRAllocators:     tt.fields.v6ClusterCIDRs,
				nodes:                tt.fields.nodes,
				netResourceSetsToK8s: tt.fields.netResourceSetsToK8s,
			}
			n.Delete(tt.args.nodeName)

			if tt.testPostRun != nil {
				tt.testPostRun(tt.fields)
			}
		}
	})
}

// podCidrTestContext creates a context for pod CIDR tests.
func podCidrTestContext(t *testing.T) (ipam.NetResourceSetGetterUpdater, error) {
	ccemock.NewMockClient()
	nrsGetterUpdater := watchers.NetResourceSetClient

	k8sObj := ccemock.NewMockSimpleNrs("10.128.34.56", "node1")
	err := ccemock.EnsureNrsToInformer(t, []*v2.NetResourceSet{k8sObj})
	if !assert.NoError(t, err, "ensure nrs to informer failed") {
		return nil, err
	}

	k8sNode := ccemock.NewMockNodeFromNrs(k8sObj)
	err = ccemock.EnsureNodeToInformer(t, []*corev1.Node{k8sNode})
	if !assert.NoError(t, err, "ensure node to informer failed") {
		return nil, err
	}

	return nrsGetterUpdater, nil
}

func (s *PodCIDRSuite) TestNodesPodCIDRManager_Resync(c *C) {
	var reSyncCalls int32
	type fields struct {
		k8sReSync *trigger.Trigger
	}
	tests := []struct {
		testSetup   func() *fields
		testPostRun func(fields *fields)
		name        string
		fields      *fields
	}{
		{
			name: "test-1",
			testSetup: func() *fields {
				return &fields{
					k8sReSync: mustNewTrigger(func() {
						atomic.AddInt32(&reSyncCalls, 1)
						return
					}, time.Millisecond),
				}
			},
			testPostRun: func(fields *fields) {
				time.Sleep(2 * time.Millisecond)
				c.Assert(atomic.LoadInt32(&reSyncCalls), Equals, int32(1))
			},
		},
	}

	for _, tt := range tests {
		tt.fields = tt.testSetup()
		nodeGetter, err := podCidrTestContext(&testing.T{})
		if err != nil {
			c.Assert(err, IsNil)
		}
		n := &NodesPodCIDRManager{
			k8sReSync:            tt.fields.k8sReSync,
			nodeGetter:           nodeGetter,
			netResourceSetsToK8s: map[string]*netResourceSetK8sOp{},
		}
		n.Resync(context.Background(), time.Time{})

		if tt.testPostRun != nil {
			tt.testPostRun(tt.fields)
		}
	}
}

func (s *PodCIDRSuite) TestNodesPodCIDRManager_Update(c *C) {
	type fields struct {
		k8sReSyncController  *controller.Manager
		k8sReSync            *trigger.Trigger
		canAllocateNodes     bool
		v4ClusterCIDRs       []CIDRAllocator
		v6ClusterCIDRs       []CIDRAllocator
		nodes                map[string]*nodeCIDRs
		netResourceSetsToK8s map[string]*netResourceSetK8sOp
	}
	type args struct {
		node *v2.NetResourceSet
	}
	tests := []struct {
		testSetup   func() *fields
		testPostRun func(fields *fields)
		name        string
		fields      *fields
		args        args
		want        error
	}{
		{
			name: "test-1 - should allocate a v4 addr",
			want: nil,
			testSetup: func() *fields {
				return &fields{
					canAllocateNodes: true,
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnAllocateNext: func() (ipNet *net.IPNet, err error) {
								return mustNewCIDRs("10.10.0.0/24")[0], nil
							},
							OnIsFull: func() bool {
								return false
							},
						},
					},
					nodes:                map[string]*nodeCIDRs{},
					netResourceSetsToK8s: map[string]*netResourceSetK8sOp{},
					k8sReSync: mustNewTrigger(func() {
						return
					}, time.Second),
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
					},
				})
				c.Assert(fields.netResourceSetsToK8s, checker.DeepEquals, map[string]*netResourceSetK8sOp{
					"node-1": {
						netResourceSet: &v2.NetResourceSet{
							ObjectMeta: v1.ObjectMeta{
								Name:            "node-1",
								ResourceVersion: "1",
							},
							Spec: v2.NetResourceSpec{
								IPAM: ipamTypes.IPAMSpec{
									PodCIDRs: []string{
										"10.10.0.0/24",
									},
								},
							},
						},
						op: k8sOpUpdate,
					},
				})
			},
			args: args{
				node: &v2.NetResourceSet{
					ObjectMeta: v1.ObjectMeta{
						Name:            "node-1",
						ResourceVersion: "1",
					},
				},
			},
		},
		{
			name: "test-2 - failed to allocate a v4 addr",
			want: nil,
			testSetup: func() *fields {
				return &fields{
					canAllocateNodes: true,
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnAllocateNext: func() (ipNet *net.IPNet, err error) {
								return nil, fmt.Errorf("Allocator full!")
							},
							OnIsFull: func() bool {
								return false
							},
						},
					},
					nodes:                map[string]*nodeCIDRs{},
					netResourceSetsToK8s: map[string]*netResourceSetK8sOp{},
					k8sReSync: mustNewTrigger(func() {
						return
					}, time.Second),
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{})
				c.Assert(fields.netResourceSetsToK8s, checker.DeepEquals, map[string]*netResourceSetK8sOp{
					"node-1": {
						netResourceSet: &v2.NetResourceSet{
							ObjectMeta: v1.ObjectMeta{
								Name:            "node-1",
								ResourceVersion: "1",
							},
							Status: v2.NetResourceStatus{
								IPAM: ipamTypes.IPAMStatus{
									OperatorStatus: ipamTypes.OperatorStatus{
										Error: "Allocator full!",
									},
								},
							},
						},
						op: k8sOpUpdateStatus,
					},
				})
			},
			args: args{
				node: &v2.NetResourceSet{
					ObjectMeta: v1.ObjectMeta{
						Name:            "node-1",
						ResourceVersion: "1",
					},
				},
			},
		},
		{
			name: "test-3 - node is already allocated with the requested pod CIDRs",
			want: nil,
			testSetup: func() *fields {
				return &fields{
					canAllocateNodes: true,
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnAllocateNext: func() (ipNet *net.IPNet, err error) {
								return nil, fmt.Errorf("Allocator full!")
							},
							OnIsFull: func() bool {
								return false
							},
						},
					},
					nodes: map[string]*nodeCIDRs{
						"node-1": {
							v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						},
					},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
					},
				})
			},
			args: args{
				node: &v2.NetResourceSet{
					ObjectMeta: v1.ObjectMeta{
						Name:            "node-1",
						ResourceVersion: "1",
					},
					Spec: v2.NetResourceSpec{
						IPAM: ipamTypes.IPAMSpec{
							PodCIDRs: []string{
								"10.10.0.0/24",
							},
						},
					},
				},
			},
		},
		{
			name: "test-4 - node is requesting pod CIDRs, it's already allocated locally but the spec is not updated",
			want: nil,
			testSetup: func() *fields {
				return &fields{
					canAllocateNodes: true,
					nodes: map[string]*nodeCIDRs{
						"node-1": {
							v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						},
					},
					netResourceSetsToK8s: map[string]*netResourceSetK8sOp{},
					k8sReSync: mustNewTrigger(func() {
						return
					}, time.Second),
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
					},
				})
				c.Assert(fields.netResourceSetsToK8s, checker.DeepEquals, map[string]*netResourceSetK8sOp{
					"node-1": {
						netResourceSet: &v2.NetResourceSet{
							ObjectMeta: v1.ObjectMeta{
								Name:            "node-1",
								ResourceVersion: "1",
							},
							Spec: v2.NetResourceSpec{
								IPAM: ipamTypes.IPAMSpec{
									PodCIDRs: []string{
										"10.10.0.0/24",
									},
								},
							},
						},
						op: k8sOpUpdate,
					},
				})
			},
			args: args{
				node: &v2.NetResourceSet{
					ObjectMeta: v1.ObjectMeta{
						Name:            "node-1",
						ResourceVersion: "1",
					},
				},
			},
		},
	}

	runWithIPAMModes(defaultIPAMModes, func(ipamMode string) {
		for _, tt := range tests {
			c.Logf("Running %q (ipam: %s)", tt.name, ipamMode)
			tt.fields = tt.testSetup()
			n := &NodesPodCIDRManager{
				k8sReSyncController:  tt.fields.k8sReSyncController,
				k8sReSync:            tt.fields.k8sReSync,
				canAllocatePodCIDRs:  tt.fields.canAllocateNodes,
				v4CIDRAllocators:     tt.fields.v4ClusterCIDRs,
				v6CIDRAllocators:     tt.fields.v6ClusterCIDRs,
				nodes:                tt.fields.nodes,
				netResourceSetsToK8s: tt.fields.netResourceSetsToK8s,
			}
			got := n.Update(tt.args.node)
			c.Assert(got, checker.Equals, tt.want, Commentf("Test Name: %s", tt.name))

			if tt.testPostRun != nil {
				tt.testPostRun(tt.fields)
			}
		}
	})
}

func (s *PodCIDRSuite) TestNodesPodCIDRManager_allocateIPNets(c *C) {
	var (
		onOccupyCallsv4, releaseCallsv4, onIsAllocatedCallsv4 int
		onOccupyCallsv6, releaseCallsv6, onIsAllocatedCallsv6 int
		onAllocateNextv6                                      int
	)

	type fields struct {
		canAllocatePodCIDRs bool
		v4ClusterCIDRs      []CIDRAllocator
		v6ClusterCIDRs      []CIDRAllocator
		newNodeCIDRs        *nodeCIDRs
		nodes               map[string]*nodeCIDRs
	}
	type args struct {
		nodeName string
		v4CIDR   []*net.IPNet
		v6CIDR   []*net.IPNet
	}
	tests := []struct {
		name          string
		testSetup     func() *fields
		testPostRun   func(fields *fields)
		fields        *fields
		args          args
		wantAllocated bool
		wantErr       bool
	}{
		{
			name: "test-1 - should not allocate anything because the node had previously allocated CIDRs",
			testSetup: func() *fields {
				return &fields{
					canAllocatePodCIDRs: true,
					v4ClusterCIDRs:      []CIDRAllocator{&mockCIDRAllocator{}},
					v6ClusterCIDRs:      []CIDRAllocator{&mockCIDRAllocator{}},
					nodes: map[string]*nodeCIDRs{
						"node-1": {
							v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
							v6PodCIDRs: mustNewCIDRs("fd00::/80"),
						},
					},
					newNodeCIDRs: &nodeCIDRs{
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						v6PodCIDRs: mustNewCIDRs("fd00::/80"),
					},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						v6PodCIDRs: mustNewCIDRs("fd00::/80"),
					},
				})
			},
			args: args{
				nodeName: "node-1",
				v4CIDR:   mustNewCIDRs("10.10.0.0/24"),
				v6CIDR:   mustNewCIDRs("fd00::/80"),
			},
			wantAllocated: false,
			wantErr:       false,
		},
		{
			name: "test-2 - should allocate both CIDRs",
			testSetup: func() *fields {
				releaseCallsv4, releaseCallsv6 = 0, 0
				onOccupyCallsv4, onOccupyCallsv6 = 0, 0
				onIsAllocatedCallsv4, onIsAllocatedCallsv6 = 0, 0
				return &fields{
					canAllocatePodCIDRs: true,
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnOccupy: func(cidr *net.IPNet) error {
								onOccupyCallsv4++
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								return nil
							},
							OnIsAllocated: func(cidr *net.IPNet) (bool, error) {
								onIsAllocatedCallsv4++
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								return false, nil
							},
							OnInRange: func(cidr *net.IPNet) bool {
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								return true
							},
							OnIsFull: func() bool {
								return false
							},
						},
					},
					v6ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnOccupy: func(cidr *net.IPNet) error {
								onOccupyCallsv6++
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("fd00::/80")[0])
								return nil
							},
							OnIsAllocated: func(cidr *net.IPNet) (bool, error) {
								onIsAllocatedCallsv6++
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("fd00::/80")[0])
								return false, nil
							},
							OnInRange: func(cidr *net.IPNet) bool {
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("fd00::/80")[0])
								return true
							},
							OnIsFull: func() bool {
								return false
							},
						},
					},
					nodes: map[string]*nodeCIDRs{},
					newNodeCIDRs: &nodeCIDRs{
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						v6PodCIDRs: mustNewCIDRs("fd00::/80"),
					},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						v6PodCIDRs: mustNewCIDRs("fd00::/80"),
					},
				})
				c.Assert(onIsAllocatedCallsv4, Equals, 1)
				c.Assert(onOccupyCallsv4, Equals, 1)
				c.Assert(releaseCallsv4, Equals, 0)

				c.Assert(onIsAllocatedCallsv6, Equals, 1)
				c.Assert(onOccupyCallsv6, Equals, 1)
				c.Assert(releaseCallsv6, Equals, 0)
			},
			args: args{
				nodeName: "node-1",
				v4CIDR:   mustNewCIDRs("10.10.0.0/24"),
				v6CIDR:   mustNewCIDRs("fd00::/80"),
			},
			wantAllocated: true,
			wantErr:       false,
		},
		{
			name: "test-3 - the v6 allocator is full!",
			testSetup: func() *fields {
				releaseCallsv4, releaseCallsv6 = 0, 0
				onOccupyCallsv4, onOccupyCallsv6 = 0, 0
				onIsAllocatedCallsv4, onIsAllocatedCallsv6 = 0, 0
				return &fields{
					canAllocatePodCIDRs: true,
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnIsAllocated: func(cidr *net.IPNet) (bool, error) {
								onIsAllocatedCallsv4++
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								return false, nil
							},
							OnOccupy: func(cidr *net.IPNet) error {
								onOccupyCallsv4++
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								return nil
							},
							OnRelease: func(cidr *net.IPNet) error {
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								releaseCallsv4++
								return nil
							},
							OnInRange: func(cidr *net.IPNet) bool {
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								return true
							},
							OnIsFull: func() bool {
								return false
							},
						},
					},
					v6ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnInRange: func(cidr *net.IPNet) bool {
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("fd00::/80")[0])
								return true
							},
							OnIsFull: func() bool {
								return true
							},
						},
					},
					nodes: map[string]*nodeCIDRs{},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{})
				c.Assert(onIsAllocatedCallsv4, Equals, 1)
				c.Assert(onOccupyCallsv4, Equals, 1)
				c.Assert(releaseCallsv4, Equals, 1)

				c.Assert(onIsAllocatedCallsv6, Equals, 0)
				c.Assert(onOccupyCallsv6, Equals, 0)
				c.Assert(releaseCallsv6, Equals, 0)
			},
			args: args{
				nodeName: "node-1",
				v4CIDR:   mustNewCIDRs("10.10.0.0/24"),
				v6CIDR:   mustNewCIDRs("fd00::/80"),
			},
			wantAllocated: false,
			wantErr:       true,
		},
		{
			name: "test-4 - changing CIDRs of a node is not valid",
			testSetup: func() *fields {
				return &fields{
					canAllocatePodCIDRs: true,
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{},
					},
					v6ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{},
					},
					nodes: map[string]*nodeCIDRs{
						"node-1": {
							v4PodCIDRs: mustNewCIDRs("10.10.1.0/24"),
							v6PodCIDRs: mustNewCIDRs("fd01::/80"),
						},
					},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.1.0/24"),
						v6PodCIDRs: mustNewCIDRs("fd01::/80"),
					},
				})
			},
			args: args{
				nodeName: "node-1",
				v4CIDR:   mustNewCIDRs("10.10.0.0/24"),
				v6CIDR:   mustNewCIDRs("fd00::/80"),
			},
			wantAllocated: false,
			wantErr:       true,
		},
		{
			name: "test-5 - should not allocate anything because there isn't" +
				" an allocator available for the CIDR family requested!",
			testSetup: func() *fields {
				return &fields{
					canAllocatePodCIDRs: true,
					nodes: map[string]*nodeCIDRs{
						"node-1": {
							v4PodCIDRs: mustNewCIDRs("10.10.1.0/24"),
							v6PodCIDRs: mustNewCIDRs("fd01::/80"),
						},
					},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.1.0/24"),
						v6PodCIDRs: mustNewCIDRs("fd01::/80"),
					},
				})
			},
			args: args{
				nodeName: "node-1",
				v4CIDR:   mustNewCIDRs("10.10.1.0/24"),
				v6CIDR:   mustNewCIDRs("fd01::/80"),
			},
			wantErr: true,
		},
		{
			name: "test-7- should allocate a v6 address if the node has a v4 " +
				"and missing a v6 address.",
			testSetup: func() *fields {
				onAllocateNextv6 = 0
				return &fields{
					canAllocatePodCIDRs: true,
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{},
					},
					v6ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnIsFull: func() bool {
								return false
							},
							OnAllocateNext: func() (*net.IPNet, error) {
								onAllocateNextv6++
								return mustNewCIDRs("fd00::/80")[0], nil
							},
						},
					},
					nodes: map[string]*nodeCIDRs{
						"node-1": {
							v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						},
					},
					newNodeCIDRs: &nodeCIDRs{
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						v6PodCIDRs: mustNewCIDRs("fd00::/80"),
					},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						v6PodCIDRs: mustNewCIDRs("fd00::/80"),
					},
				})
				c.Assert(onAllocateNextv6, Equals, 1)
			},
			args: args{
				nodeName: "node-1",
				v4CIDR:   mustNewCIDRs("10.10.0.0/24"),
			},
			wantAllocated: true,
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		tt.fields = tt.testSetup()
		n := &NodesPodCIDRManager{
			canAllocatePodCIDRs: tt.fields.canAllocatePodCIDRs,
			v4CIDRAllocators:    tt.fields.v4ClusterCIDRs,
			v6CIDRAllocators:    tt.fields.v6ClusterCIDRs,
			nodes:               tt.fields.nodes,
		}
		newNodeCIDRs, gotAllocated, err := n.reuseIPNets(tt.args.nodeName, tt.args.v4CIDR, tt.args.v6CIDR)
		gotErr := err != nil
		c.Assert(gotErr, Equals, tt.wantErr, Commentf("Test Name: %s", tt.name))
		c.Assert(gotAllocated, Equals, tt.wantAllocated, Commentf("Test Name: %s", tt.name))
		c.Assert(newNodeCIDRs, checker.DeepEquals, tt.fields.newNodeCIDRs, Commentf("Test Name: %s", tt.name))

		if tt.testPostRun != nil {
			tt.testPostRun(tt.fields)
		}
	}
}

func (s *PodCIDRSuite) TestNodesPodCIDRManager_allocateNext(c *C) {
	var (
		allocateNextCallsv4, releaseCallsv4 int
		allocateNextCallsv6                 int
	)

	type fields struct {
		v4ClusterCIDRs []CIDRAllocator
		v6ClusterCIDRs []CIDRAllocator
		nodes          map[string]*nodeCIDRs
	}
	type args struct {
		nodeName string
	}
	tests := []struct {
		testSetup     func() *fields
		testPostRun   func(fields *fields)
		name          string
		fields        *fields
		args          args
		nodeCIDRs     *nodeCIDRs
		wantAllocated bool
		wantErr       error
	}{
		{
			name: "test-1 - should not allocate anything because the node had previously allocated CIDRs",
			testSetup: func() *fields {
				return &fields{
					v4ClusterCIDRs: []CIDRAllocator{},
					v6ClusterCIDRs: []CIDRAllocator{},
					nodes: map[string]*nodeCIDRs{
						"node-1": {
							v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
							v6PodCIDRs: mustNewCIDRs("fd00::/80"),
						},
					},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						v6PodCIDRs: mustNewCIDRs("fd00::/80"),
					},
				})
			},
			args: args{
				nodeName: "node-1",
			},
			nodeCIDRs: &nodeCIDRs{
				v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
				v6PodCIDRs: mustNewCIDRs("fd00::/80"),
			},
			wantAllocated: false,
			wantErr:       nil,
		},
		{
			name: "test-2 - should allocate both CIDRs",
			testSetup: func() *fields {
				allocateNextCallsv4, allocateNextCallsv6 = 0, 0
				return &fields{
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnAllocateNext: func() (ipNet *net.IPNet, err error) {
								allocateNextCallsv4++
								return mustNewCIDRs("10.10.0.0/24")[0], nil
							},
							OnIsFull: func() bool {
								return false
							},
							OnInRange: func(cidr *net.IPNet) bool {
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								return true
							},
						},
					},
					v6ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnAllocateNext: func() (ipNet *net.IPNet, err error) {
								allocateNextCallsv6++
								return mustNewCIDRs("fd00::/80")[0], nil
							},
							OnIsFull: func() bool {
								return false
							},
							OnInRange: func(cidr *net.IPNet) bool {
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								return true
							},
						},
					},
					nodes: map[string]*nodeCIDRs{},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{
					"node-1": {
						v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
						v6PodCIDRs: mustNewCIDRs("fd00::/80"),
					},
				})
				c.Assert(allocateNextCallsv4, Equals, 1)
				c.Assert(allocateNextCallsv6, Equals, 1)
			},
			args: args{
				nodeName: "node-1",
			},
			nodeCIDRs: &nodeCIDRs{
				v4PodCIDRs: mustNewCIDRs("10.10.0.0/24"),
				v6PodCIDRs: mustNewCIDRs("fd00::/80"),
			},
			wantAllocated: true,
			wantErr:       nil,
		},
		{
			name: "test-3 - the v6 allocator is full!",
			testSetup: func() *fields {
				allocateNextCallsv4 = 0
				releaseCallsv4 = 0
				return &fields{
					v4ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnAllocateNext: func() (ipNet *net.IPNet, err error) {
								allocateNextCallsv4++
								return mustNewCIDRs("10.10.0.0/24")[0], nil
							},
							OnRelease: func(cidr *net.IPNet) error {
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								releaseCallsv4++
								return nil
							},
							OnIsFull: func() bool {
								return false
							},
							OnInRange: func(cidr *net.IPNet) bool {
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								return true
							},
						},
					},
					v6ClusterCIDRs: []CIDRAllocator{
						&mockCIDRAllocator{
							OnIsFull: func() bool {
								return true
							},
							OnInRange: func(cidr *net.IPNet) bool {
								c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.10.0.0/24")[0])
								return true
							},
						},
					},
					nodes: map[string]*nodeCIDRs{},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, checker.DeepEquals, map[string]*nodeCIDRs{})
				c.Assert(allocateNextCallsv4, Equals, 1)
				c.Assert(releaseCallsv4, Equals, 1)
			},
			args: args{
				nodeName: "node-1",
			},
			wantAllocated: false,
			wantErr:       &ErrAllocatorFull{},
		},
		{
			name: "test-4 - no allocators!",
			testSetup: func() *fields {
				return &fields{
					v4ClusterCIDRs: []CIDRAllocator{},
					nodes:          map[string]*nodeCIDRs{},
				}
			},
			args: args{
				nodeName: "node-1",
			},
			wantAllocated: false,
			wantErr: ErrNoAllocators{
				name: "node-1",
				v4:   "[]",
				v6:   "[]",
			},
		},
	}

	for _, tt := range tests {
		tt.fields = tt.testSetup()
		n := &NodesPodCIDRManager{
			v4CIDRAllocators: tt.fields.v4ClusterCIDRs,
			v6CIDRAllocators: tt.fields.v6ClusterCIDRs,
			nodes:            tt.fields.nodes,
		}
		nodeCIDRs, gotAllocated, err := n.allocateNext(tt.args.nodeName)
		c.Assert(err, checker.DeepEquals, tt.wantErr, Commentf("Test Name: %s", tt.name))
		c.Assert(nodeCIDRs, checker.DeepEquals, tt.nodeCIDRs, Commentf("Test Name: %s", tt.name))
		c.Assert(gotAllocated, Equals, tt.wantAllocated, Commentf("Test Name: %s", tt.name))

		if tt.testPostRun != nil {
			tt.testPostRun(tt.fields)
		}
	}
}

func (s *PodCIDRSuite) TestNodesPodCIDRManager_releaseIPNets(c *C) {
	var onReleaseCalls int

	type fields struct {
		v4ClusterCIDRs []CIDRAllocator
		v6ClusterCIDRs []CIDRAllocator
		nodes          map[string]*nodeCIDRs
	}
	type args struct {
		nodeName string
	}
	tests := []struct {
		testSetup   func() *fields
		testPostRun func(fields *fields)
		name        string
		fields      *fields
		args        args
		want        bool
	}{
		{
			name: "test-1",
			testSetup: func() *fields {
				return &fields{
					nodes: map[string]*nodeCIDRs{},
				}
			},
			args: args{
				nodeName: "node-1",
			},
			want: false,
		},
		{
			name: "test-2",
			testSetup: func() *fields {
				onReleaseCalls = 0
				cidrSet := []CIDRAllocator{
					&mockCIDRAllocator{
						OnRelease: func(cidr *net.IPNet) error {
							onReleaseCalls++
							c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.0.0.0/16")[0])
							return nil
						},
						OnInRange: func(cidr *net.IPNet) bool {
							c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("10.0.0.0/16")[0])
							return true
						},
					},
				}
				return &fields{
					v4ClusterCIDRs: cidrSet,
					nodes: map[string]*nodeCIDRs{
						"node-1": {
							v4PodCIDRs: mustNewCIDRs("10.0.0.0/16"),
						},
					},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, HasLen, 0)
				c.Assert(onReleaseCalls, Equals, 1)
			},
			args: args{
				nodeName: "node-1",
			},
			want: true,
		},
		{
			name: "test-3",
			testSetup: func() *fields {
				onReleaseCalls = 0
				cidrSet := []CIDRAllocator{
					&mockCIDRAllocator{
						OnRelease: func(cidr *net.IPNet) error {
							onReleaseCalls++
							c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("fd00::/80")[0])
							return nil
						},
						OnInRange: func(cidr *net.IPNet) bool {
							c.Assert(cidr, checker.DeepEquals, mustNewCIDRs("fd00::/80")[0])
							return true
						},
					},
				}
				return &fields{
					v6ClusterCIDRs: cidrSet,
					nodes: map[string]*nodeCIDRs{
						"node-1": {
							v6PodCIDRs: mustNewCIDRs("fd00::/80"),
						},
					},
				}
			},
			testPostRun: func(fields *fields) {
				c.Assert(fields.nodes, HasLen, 0)
				c.Assert(onReleaseCalls, Equals, 1)
			},
			args: args{
				nodeName: "node-1",
			},
			want: true,
		},
	}
	for _, tt := range tests {
		tt.fields = tt.testSetup()
		n := &NodesPodCIDRManager{
			v4CIDRAllocators: tt.fields.v4ClusterCIDRs,
			v6CIDRAllocators: tt.fields.v6ClusterCIDRs,
			nodes:            tt.fields.nodes,
		}
		got := n.releaseIPNets(tt.args.nodeName)
		c.Assert(got, checker.Equals, tt.want, Commentf("Test Name: %s", tt.name))

		if tt.testPostRun != nil {
			tt.testPostRun(tt.fields)
		}
	}
}

func (s *PodCIDRSuite) Test_parsePodCIDRs(c *C) {
	type args struct {
		podCIDRs []string
	}
	tests := []struct {
		name    string
		args    args
		want    *nodeCIDRs
		wantErr bool
	}{
		{
			name: "test-1",
			args: args{
				podCIDRs: []string{
					"1.1.1.1/20",
					"1.1.1.1/28",
				},
			},
			want: &nodeCIDRs{
				v4PodCIDRs: mustNewCIDRs("1.1.1.1/20", "1.1.1.1/28"),
			},
			wantErr: false,
		},
		{
			name: "test-2",
			args: args{
				podCIDRs: []string{
					"fd00::1/64",
					"fd01::/64",
				},
			},
			want: &nodeCIDRs{
				v6PodCIDRs: mustNewCIDRs("fd00::1/64", "fd01::/64"),
			},
			wantErr: false,
		},
		{
			name: "test-3",
			args: args{
				podCIDRs: []string{
					"fd00::1/64",
					"1.1.1.1/28",
				},
			},
			want: &nodeCIDRs{
				v4PodCIDRs: mustNewCIDRs("1.1.1.0/28"),
				v6PodCIDRs: mustNewCIDRs("fd00::/64"),
			},
			wantErr: false,
		},
		{
			name: "test-4",
			args: args{
				podCIDRs: []string{
					"fd00::1/64",
				},
			},
			want: &nodeCIDRs{
				v6PodCIDRs: mustNewCIDRs("fd00::/64"),
			},
			wantErr: false,
		},
		{
			name: "test-5",
			args: args{
				podCIDRs: []string{
					"1.1.1.1/28",
				},
			},
			want: &nodeCIDRs{
				v4PodCIDRs: mustNewCIDRs("1.1.1.0/28"),
			},
			wantErr: false,
		},
		{
			name: "test-6",
			args: args{
				podCIDRs: []string{
					"1.1.1.1/280",
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		nodeCIDRs, err := parsePodCIDRs(tt.args.podCIDRs)
		gotErr := err != nil
		c.Assert(gotErr, Equals, tt.wantErr, Commentf("Test Name: %s", tt.name))
		c.Assert(nodeCIDRs, checker.DeepEquals, tt.want, Commentf("Test Name: %s", tt.name))
	}
}
